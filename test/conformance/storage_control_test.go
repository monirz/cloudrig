package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/storage"
	control "cloud.google.com/go/storage/control/apiv2"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/monirz/cloudrig"
)

// controlClient starts an emulator with a bucket in it and returns the real
// Storage Control client pointed at it, plus the emulator's HTTP base.
func controlClient(t *testing.T) (*control.StorageControlClient, context.Context, string) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()
	gcs, err := storage.NewClient(ctx, option.WithEndpoint(emu.BaseURL()+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gcs.Close() })
	if err := gcs.Bucket("bkt").Create(ctx, "p", nil); err != nil {
		t.Fatalf("creating the bucket: %v", err)
	}

	c, err := control.NewStorageControlClient(ctx,
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("control.NewStorageControlClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx, emu.BaseURL()
}

const bucketName = "projects/_/buckets/bkt"

func TestManagedFolderCreateGetList(t *testing.T) {
	c, ctx, base := controlClient(t)

	f, err := c.CreateManagedFolder(ctx, &controlpb.CreateManagedFolderRequest{
		Parent: bucketName, ManagedFolderId: "tenants/acme",
	})
	if err != nil {
		t.Fatalf("CreateManagedFolder: %v", err)
	}
	if want := bucketName + "/managedFolders/tenants/acme/"; f.GetName() != want {
		t.Errorf("name = %q, want %q (a trailing slash is added)", f.GetName(), want)
	}
	if f.GetMetageneration() != 1 || f.GetCreateTime() == nil {
		t.Errorf("created %v, want metageneration 1 and a create time", f)
	}

	_, err = c.CreateManagedFolder(ctx, &controlpb.CreateManagedFolderRequest{
		Parent: bucketName, ManagedFolderId: "tenants/acme/",
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("repeat create: %v, want AlreadyExists", err)
	}

	got, err := c.GetManagedFolder(ctx, &controlpb.GetManagedFolderRequest{Name: f.GetName()})
	if err != nil {
		t.Fatalf("GetManagedFolder: %v", err)
	}
	if !proto.Equal(got, f) {
		t.Errorf("get = %v, want %v", got, f)
	}
	for name, want := range map[string]codes.Code{
		bucketName + "/managedFolders/nope/":           codes.NotFound,
		"projects/_/buckets/missing/managedFolders/a/": codes.NotFound,
		"buckets/bkt/managedFolders/a/":                codes.InvalidArgument,
	} {
		if _, err := c.GetManagedFolder(ctx, &controlpb.GetManagedFolderRequest{Name: name}); status.Code(err) != want {
			t.Errorf("get %s: %v, want %s", name, err, want)
		}
	}
	one := int64(2)
	if _, err := c.GetManagedFolder(ctx, &controlpb.GetManagedFolderRequest{Name: f.GetName(), IfMetagenerationMatch: &one}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("get with a stale metageneration: %v, want FailedPrecondition", err)
	}

	// A folder made over gRPC is visible to the JSON API.
	if _, err := c.CreateManagedFolder(ctx, &controlpb.CreateManagedFolderRequest{Parent: bucketName, ManagedFolderId: "logs"}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(base + "/storage/v1/b/bkt/managedFolders")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Kind  string `json:"kind"`
		Items []struct {
			Kind, Name, Bucket, Metageneration string
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Items[0].Name != "logs/" || list.Items[1].Name != "tenants/acme/" ||
		list.Items[1].Kind != "storage#managedFolder" || list.Items[1].Bucket != "bkt" || list.Items[1].Metageneration != "1" {
		t.Errorf("JSON list = %+v, want logs/ and tenants/acme/", list)
	}
}

func TestManagedFolderIAMPolicy(t *testing.T) {
	c, ctx, _ := controlClient(t)
	resource := bucketName + "/managedFolders/tenants/acme/"
	if _, err := c.CreateManagedFolder(ctx, &controlpb.CreateManagedFolderRequest{Parent: bucketName, ManagedFolderId: "tenants/acme/"}); err != nil {
		t.Fatal(err)
	}

	empty, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	if len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
		t.Errorf("fresh policy = %v, want no bindings and an etag", empty)
	}

	want := &iampb.Policy{
		Version: 3,
		Etag:    empty.GetEtag(),
		Bindings: []*iampb.Binding{
			{Role: "roles/storage.objectViewer", Members: []string{"user:a@example.com"}},
			{
				Role:    "roles/storage.objectAdmin",
				Members: []string{"serviceAccount:worker@p.iam.gserviceaccount.com"},
				Condition: &expr.Expr{
					Title:      "until-review",
					Expression: `request.time < timestamp("2027-01-01T00:00:00Z")`,
				},
			},
		},
	}
	set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: want})
	if err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{
		Resource: resource, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, set) {
		t.Errorf("get after set = %v, want %v", got, set)
	}
	if got.GetVersion() != 3 || !proto.Equal(got.GetBindings()[1].GetCondition(), want.GetBindings()[1].GetCondition()) {
		t.Errorf("policy = %v, want version 3 with the condition kept", got)
	}

	// The etag from before the write is stale now.
	_, err = c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: want})
	if status.Code(err) != codes.Aborted {
		t.Errorf("set with a stale etag: %v, want Aborted", err)
	}
	// A condition needs version 3.
	v1 := proto.Clone(want).(*iampb.Policy)
	v1.Version, v1.Etag = 1, nil
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: v1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("conditions at version 1: %v, want InvalidArgument", err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: bucketName + "/managedFolders/nope/"}); status.Code(err) != codes.NotFound {
		t.Errorf("policy of a missing folder: %v, want NotFound", err)
	}
}

func TestStorageControlOutOfScope(t *testing.T) {
	c, ctx, _ := controlClient(t)
	if _, err := c.CreateFolder(ctx, &controlpb.CreateFolderRequest{Parent: bucketName, FolderId: "x/"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateFolder: %v, want Unimplemented", err)
	}
	if _, err := c.GetStorageLayout(ctx, &controlpb.GetStorageLayoutRequest{Name: bucketName + "/storageLayout"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("GetStorageLayout: %v, want Unimplemented", err)
	}
}

// gcloud storage rm -r lists a bucket's managed folders, deletes each over the
// JSON API, then deletes the bucket; the Python client deletes over gRPC.
func TestManagedFolderDelete(t *testing.T) {
	c, ctx, base := controlClient(t)
	for _, id := range []string{"tenants/acme/", "logs/"} {
		if _, err := c.CreateManagedFolder(ctx, &controlpb.CreateManagedFolderRequest{Parent: bucketName, ManagedFolderId: id}); err != nil {
			t.Fatal(err)
		}
	}
	del := func(path string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := del("/storage/v1/b/bkt"); got != http.StatusConflict {
		t.Errorf("deleting a bucket holding folders = %d, want 409", got)
	}
	if got := del("/storage/v1/b/bkt/managedFolders/tenants%2Facme%2F"); got != http.StatusNoContent {
		t.Errorf("JSON delete = %d, want 204", got)
	}
	wrong := int64(7)
	if err := c.DeleteManagedFolder(ctx, &controlpb.DeleteManagedFolderRequest{Name: bucketName + "/managedFolders/logs/", IfMetagenerationMatch: &wrong}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("gRPC delete with a wrong metageneration: %v, want FailedPrecondition", err)
	}
	if got := del("/storage/v1/b/bkt/managedFolders/logs%2F?ifMetagenerationMatch=7"); got != http.StatusPreconditionFailed {
		t.Errorf("JSON delete with a wrong metageneration = %d, want 412", got)
	}
	if err := c.DeleteManagedFolder(ctx, &controlpb.DeleteManagedFolderRequest{Name: bucketName + "/managedFolders/logs/"}); err != nil {
		t.Errorf("gRPC delete: %v", err)
	}
	if err := c.DeleteManagedFolder(ctx, &controlpb.DeleteManagedFolderRequest{Name: bucketName + "/managedFolders/logs/"}); status.Code(err) != codes.NotFound {
		t.Errorf("second delete: %v, want NotFound", err)
	}
	if got := del("/storage/v1/b/bkt"); got != http.StatusNoContent {
		t.Errorf("deleting the emptied bucket = %d, want 204", got)
	}
}
