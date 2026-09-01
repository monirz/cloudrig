package secretmanager

import (
	"context"
	"encoding/json"
	"errors"
	"hash/crc32"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Service) CreateSecret(ctx context.Context, req *secretmanagerpb.CreateSecretRequest) (*secretmanagerpb.Secret, error) {
	project := strings.TrimPrefix(req.GetParent(), "projects/")
	if project == "" || strings.Contains(project, "/") {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid parent %q; expected projects/{project}", req.GetParent())
	}
	if req.GetSecretId() == "" {
		return nil, status.Error(codes.InvalidArgument, "secretId is required")
	}

	secret := req.GetSecret()
	if secret == nil {
		secret = &secretmanagerpb.Secret{}
	}
	secret.Name = req.GetParent() + "/secrets/" + req.GetSecretId()
	secret.CreateTime = timestamppb.New(s.clk.Now())
	if secret.GetReplication() == nil {
		// Automatic replication is what a local emulator can honestly claim;
		// there is one place for the value to live.
		secret.Replication = &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{
				Automatic: &secretmanagerpb.Replication_Automatic{},
			},
		}
	}

	encoded, err := marshal.Marshal(secret)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding secret: %v", err)
	}
	// ifVersion 0 makes the duplicate check and the write one step.
	if _, err := s.kv.Put(ctx, secretKey(secret.Name), encoded, 0); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return nil, status.Errorf(codes.AlreadyExists, "Secret [%s] already exists", secret.Name)
		}
		return nil, status.Errorf(codes.Internal, "storing secret: %v", err)
	}
	return secret, nil
}

func (s *Service) GetSecret(ctx context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	if _, _, err := parseSecret(req.GetName()); err != nil {
		return nil, err
	}
	return s.getSecret(ctx, req.GetName())
}

func (s *Service) ListSecrets(ctx context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	entries, _, err := s.kv.List(ctx, "sm/s/"+req.GetParent()+"/secrets/", 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing secrets: %v", err)
	}

	out := make([]*secretmanagerpb.Secret, 0, len(entries))
	for _, kv := range entries {
		var secret secretmanagerpb.Secret
		if err := unmarshal.Unmarshal(kv.Val, &secret); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding secret: %v", err)
		}
		out = append(out, &secret)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return &secretmanagerpb.ListSecretsResponse{Secrets: out, TotalSize: int32(len(out))}, nil
}

func (s *Service) DeleteSecret(ctx context.Context, req *secretmanagerpb.DeleteSecretRequest) (*emptypb.Empty, error) {
	if _, _, err := parseSecret(req.GetName()); err != nil {
		return nil, err
	}
	if err := s.kv.Delete(ctx, secretKey(req.GetName()), 0); err != nil {
		return nil, status.Errorf(codes.NotFound, "Secret [%s] not found", req.GetName())
	}

	// The versions go with it: a secret's value has nowhere else to live, and
	// leaving them would resurrect the old value under a recreated name.
	entries, _, err := s.kv.List(ctx, versionPrefix(req.GetName()), 0, "")
	if err == nil {
		for _, kv := range entries {
			_ = s.kv.Delete(ctx, kv.Key, 0)
		}
	}
	return &emptypb.Empty{}, nil
}

// AddSecretVersion stores a new value. Versions are numbered from one and
// never reused, so a reference to a version always means the same bytes.
func (s *Service) AddSecretVersion(ctx context.Context, req *secretmanagerpb.AddSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	if _, _, err := parseSecret(req.GetParent()); err != nil {
		return nil, err
	}
	if _, err := s.getSecret(ctx, req.GetParent()); err != nil {
		return nil, err
	}

	payload := req.GetPayload().GetData()
	if got := req.GetPayload().GetDataCrc32C(); got != 0 {
		if want := int64(crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli))); got != want {
			return nil, status.Error(codes.InvalidArgument,
				"the payload checksum does not match its data")
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	next, err := s.nextVersion(ctx, req.GetParent())
	if err != nil {
		return nil, err
	}

	version := &secretmanagerpb.SecretVersion{
		Name:       req.GetParent() + "/versions/" + strconv.FormatInt(next, 10),
		CreateTime: timestamppb.New(s.clk.Now()),
		State:      secretmanagerpb.SecretVersion_ENABLED,
		ReplicationStatus: &secretmanagerpb.ReplicationStatus{
			ReplicationStatus: &secretmanagerpb.ReplicationStatus_Automatic{
				Automatic: &secretmanagerpb.ReplicationStatus_AutomaticStatus{},
			},
		},
	}
	if err := s.putVersion(ctx, req.GetParent(), next, &storedVersion{Version: version, Payload: payload}); err != nil {
		return nil, err
	}
	return version, nil
}

// nextVersion is one past the highest number ever used, including destroyed
// ones. The caller holds the lock.
func (s *Service) nextVersion(ctx context.Context, secret string) (int64, error) {
	entries, _, err := s.kv.List(ctx, versionPrefix(secret), 0, "")
	if err != nil {
		return 0, status.Errorf(codes.Internal, "listing versions: %v", err)
	}
	if len(entries) == 0 {
		return 1, nil
	}

	highest := entries[len(entries)-1].Key
	n, err := strconv.ParseInt(highest[strings.LastIndex(highest, "/")+1:], 10, 64)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "unreadable version key %q", highest)
	}
	return n + 1, nil
}

func (s *Service) putVersion(ctx context.Context, secret string, n int64, stored *storedVersion) error {
	encoded, err := json.Marshal(stored)
	if err != nil {
		return status.Errorf(codes.Internal, "encoding version: %v", err)
	}
	if _, err := s.kv.Put(ctx, versionKey(secret, n), encoded, 0); err != nil {
		// A version already there means another writer won; the caller holds
		// the lock, so this is a store the emulator does not own alone.
		if _, _, getErr := s.kv.Get(ctx, versionKey(secret, n)); getErr == nil {
			return status.Errorf(codes.Aborted, "version %d already exists", n)
		}
		return status.Errorf(codes.Internal, "storing version: %v", err)
	}
	return nil
}

// AccessSecretVersion returns the value. This is the call everything else
// exists to serve.
func (s *Service) AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	secret, version, err := parseVersion(req.GetName())
	if err != nil {
		return nil, err
	}

	stored, err := s.resolve(ctx, secret, version)
	if err != nil {
		return nil, err
	}
	// A disabled or destroyed version still exists, and still refuses to be
	// read: that is the difference between revoking a value and deleting it.
	if state := stored.Version.GetState(); state != secretmanagerpb.SecretVersion_ENABLED {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Secret Version [%s] is not enabled: it is %s", stored.Version.GetName(), state)
	}

	return &secretmanagerpb.AccessSecretVersionResponse{
		Name: stored.Version.GetName(),
		Payload: &secretmanagerpb.SecretPayload{
			Data: stored.Payload,
		},
	}, nil
}

func (s *Service) GetSecretVersion(ctx context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	secret, version, err := parseVersion(req.GetName())
	if err != nil {
		return nil, err
	}
	stored, err := s.resolve(ctx, secret, version)
	if err != nil {
		return nil, err
	}
	return stored.Version, nil
}

func (s *Service) ListSecretVersions(ctx context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	if _, err := s.getSecret(ctx, req.GetParent()); err != nil {
		return nil, err
	}
	versions, err := s.versions(ctx, req.GetParent())
	if err != nil {
		return nil, err
	}

	// Newest first, as the API returns them.
	sort.Slice(versions, func(i, j int) bool { return versions[i].GetName() > versions[j].GetName() })
	return &secretmanagerpb.ListSecretVersionsResponse{
		Versions: versions, TotalSize: int32(len(versions)),
	}, nil
}

// versionNumber reads the trailing number from a version's resource name.
func versionNumber(name string) (int64, error) {
	n, err := strconv.ParseInt(name[strings.LastIndex(name, "/")+1:], 10, 64)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "unreadable version name %q", name)
	}
	return n, nil
}

// replaceVersion overwrites a version in place, at whatever version the store
// holds: the caller has already read it under the lock.
func (s *Service) replaceVersion(ctx context.Context, secret string, n int64, stored *storedVersion) error {
	encoded, err := json.Marshal(stored)
	if err != nil {
		return status.Errorf(codes.Internal, "encoding version: %v", err)
	}

	key := versionKey(secret, n)
	_, current, err := s.kv.Get(ctx, key)
	if err != nil {
		return status.Errorf(codes.NotFound, "Secret Version [%s] not found", stored.Version.GetName())
	}
	if _, err := s.kv.Put(ctx, key, encoded, current); err != nil {
		return status.Errorf(codes.Aborted, "updating version: %v", err)
	}
	return nil
}
