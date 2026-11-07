package storage

import (
	"context"
	"encoding/binary"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/monirz/cloudrig/core/gerr"
)

// Control serves the managed-folder part of Storage Control over gRPC,
// google.storage.control.v2.StorageControl, which is the transport Google's
// StorageControlClient uses by default.
//
// Folders, storage layout and caches answer Unimplemented, as does every
// managed-folder call other than create, get and the IAM pair.
type Control struct {
	controlpb.UnimplementedStorageControlServer
	svc *Service
}

// NewControl wires the gRPC face onto the same service the JSON API uses, so
// a folder created here shows in the JSON managedFolders list.
func NewControl(svc *Service) *Control { return &Control{svc: svc} }

// CreateManagedFolder creates a folder; a second create is AlreadyExists.
func (c *Control) CreateManagedFolder(ctx context.Context, req *controlpb.CreateManagedFolderRequest) (*controlpb.ManagedFolder, error) {
	if req.GetManagedFolder().GetName() != "" {
		return nil, status.Error(codes.InvalidArgument,
			"managed_folder.name must be empty: the name comes from parent and managed_folder_id")
	}
	project, bucket, err := c.parseBucket(ctx, req.GetParent())
	if err != nil {
		return nil, err
	}
	f, err := c.svc.CreateManagedFolder(ctx, project, bucket, req.GetManagedFolderId())
	if err != nil {
		return nil, grpcError(err)
	}
	return toProtoFolder(f), nil
}

// GetManagedFolder returns a folder, honouring the metageneration conditions.
func (c *Control) GetManagedFolder(ctx context.Context, req *controlpb.GetManagedFolderRequest) (*controlpb.ManagedFolder, error) {
	project, bucket, folder, err := c.parseFolder(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	f, err := c.svc.GetManagedFolder(ctx, project, bucket, folder)
	if err != nil {
		return nil, grpcError(err)
	}
	if req.IfMetagenerationMatch != nil && f.Metageneration != req.GetIfMetagenerationMatch() {
		return nil, status.Errorf(codes.FailedPrecondition, "metageneration is %d, not %d", f.Metageneration, req.GetIfMetagenerationMatch())
	}
	if req.IfMetagenerationNotMatch != nil && f.Metageneration == req.GetIfMetagenerationNotMatch() {
		return nil, status.Errorf(codes.FailedPrecondition, "metageneration is %d", f.Metageneration)
	}
	return toProtoFolder(f), nil
}

// GetIamPolicy returns a folder's policy as stored, or an empty one. The
// policy is never enforced.
func (c *Control) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	project, bucket, folder, err := c.parseFolder(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	raw, version, err := c.svc.ManagedFolderPolicy(ctx, project, bucket, folder)
	if err != nil {
		return nil, grpcError(err)
	}
	p := &iampb.Policy{Version: 1}
	if raw != nil {
		if err := proto.Unmarshal(raw, p); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding the stored policy: %v", err)
		}
	}
	p.Etag = policyEtag(version)
	return p, nil
}

// SetIamPolicy replaces a folder's policy. An etag from an earlier read makes
// it a compare-and-swap: a policy changed since is Aborted, as IAM answers.
func (c *Control) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	project, bucket, folder, err := c.parseFolder(ctx, req.GetResource())
	if err != nil {
		return nil, err
	}
	p := &iampb.Policy{}
	if req.GetPolicy() != nil {
		p = proto.Clone(req.GetPolicy()).(*iampb.Policy)
	}
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Version != 1 && p.Version != 3 {
		return nil, status.Errorf(codes.InvalidArgument, "policy version %d is not valid: use 1 or 3", p.Version)
	}
	for _, b := range p.GetBindings() {
		if b.GetCondition() != nil && p.Version < 3 {
			return nil, status.Errorf(codes.InvalidArgument,
				"the binding for %s has a condition, which needs policy version 3, not %d", b.GetRole(), p.Version)
		}
	}

	var ifVersion uint64
	if tag := p.GetEtag(); len(tag) > 0 {
		if len(tag) != 8 {
			return nil, status.Error(codes.InvalidArgument, "the etag was not issued by this server")
		}
		ifVersion = binary.BigEndian.Uint64(tag)
	} else {
		// No etag means overwrite whatever is there.
		if _, ifVersion, err = c.svc.ManagedFolderPolicy(ctx, project, bucket, folder); err != nil {
			return nil, grpcError(err)
		}
	}
	p.Etag = nil
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding the policy: %v", err)
	}
	version, err := c.svc.SetManagedFolderPolicy(ctx, project, bucket, folder, raw, ifVersion)
	if err != nil {
		return nil, grpcError(err)
	}
	p.Etag = policyEtag(version)
	return p, nil
}

// parseBucket reads projects/{project}/buckets/{bucket}. The project is
// normally "_"; the bucket's own project is what counts.
func (c *Control) parseBucket(ctx context.Context, name string) (project, bucket string, err error) {
	rest, ok := strings.CutPrefix(name, "projects/")
	if ok {
		var given string
		given, bucket, ok = strings.Cut(rest, "/buckets/")
		ok = ok && given != "" && bucket != "" && !strings.Contains(bucket, "/")
		if ok {
			project, err = c.svc.ProjectOf(ctx, bucket)
			if err != nil {
				return "", "", grpcError(err)
			}
			if given != "_" && given != project {
				return "", "", grpcError(notFoundBucket(bucket))
			}
			return project, bucket, nil
		}
	}
	return "", "", status.Errorf(codes.InvalidArgument, "%q is not projects/_/buckets/{bucket}", name)
}

// parseFolder reads projects/{project}/buckets/{bucket}/managedFolders/{folder}.
// The folder name may itself hold slashes.
func (c *Control) parseFolder(ctx context.Context, name string) (project, bucket, folder string, err error) {
	parent, folder, ok := strings.Cut(name, "/managedFolders/")
	if !ok || folder == "" {
		if !ok && strings.Contains(name, "/buckets/") {
			return "", "", "", status.Errorf(codes.Unimplemented,
				"cloudrig serves IAM on managed folders only over Storage Control; for %q use the JSON API's /iam", name)
		}
		return "", "", "", status.Errorf(codes.InvalidArgument,
			"%q is not projects/_/buckets/{bucket}/managedFolders/{folder}", name)
	}
	project, bucket, err = c.parseBucket(ctx, parent)
	return project, bucket, folder, err
}

func toProtoFolder(f ManagedFolder) *controlpb.ManagedFolder {
	return &controlpb.ManagedFolder{
		Name:           "projects/_/buckets/" + f.Bucket + "/managedFolders/" + f.Name,
		Metageneration: f.Metageneration,
		CreateTime:     timestamppb.New(f.Created),
		UpdateTime:     timestamppb.New(f.Updated),
	}
}

// policyEtag is the store version of the policy: it moves on every write, so an
// A-B-A change still reads as a change.
func policyEtag(version uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, version)
}

// grpcError carries a service error's canonical code onto the gRPC status;
// gerr's codes are numbered as gRPC's are.
func grpcError(err error) error {
	e := gerr.From(err)
	return status.Error(codes.Code(e.Code), e.Message)
}
