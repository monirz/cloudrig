// Package secretmanager is the Secret Manager emulation.
//
// A secret is a container; the value lives in numbered versions beneath it,
// and "latest" is an alias for the newest one still enabled. That indirection
// is the whole model, and the reason a rotated credential does not need the
// code reading it to change.
package secretmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// Service holds secrets and their versions.
type Service struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer

	kv  store.Store
	clk clock.Clock

	// mu serialises adding a version, which reads the highest number and
	// writes the next: two concurrent adds would otherwise claim one.
	mu sync.Mutex
}

// New wires a service.
func New(kv store.Store, clk clock.Clock) *Service {
	return &Service{kv: kv, clk: clk}
}

// Key layout. Versions are zero-padded so the store's key order is version
// order, which makes "the newest" a listing rather than a scan-and-compare.
const versionDigits = 12

func secretKey(name string) string { return "sm/s/" + name }

func versionKey(secret string, version int64) string {
	return fmt.Sprintf("sm/v/%s/%0*d", secret, versionDigits, version)
}

func versionPrefix(secret string) string { return "sm/v/" + secret + "/" }

// LatestAlias is the version name every caller uses instead of a number.
const LatestAlias = "latest"

// parseSecret splits projects/{project}/secrets/{secret}.
func parseSecret(name string) (project, secret string, err error) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "secrets" || parts[1] == "" || parts[3] == "" {
		return "", "", status.Errorf(codes.InvalidArgument,
			"invalid secret name %q; expected projects/{project}/secrets/{secret}", name)
	}
	return parts[1], parts[3], nil
}

// parseVersion splits projects/{project}/secrets/{secret}/versions/{version},
// where the version may be a number or the latest alias.
func parseVersion(name string) (secret, version string, err error) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[4] != "versions" || parts[5] == "" {
		return "", "", status.Errorf(codes.InvalidArgument,
			"invalid version name %q; expected projects/{p}/secrets/{s}/versions/{v}", name)
	}
	secret = strings.Join(parts[:4], "/")
	if _, _, err := parseSecret(secret); err != nil {
		return "", "", err
	}
	return secret, parts[5], nil
}

func (s *Service) getSecret(ctx context.Context, name string) (*secretmanagerpb.Secret, error) {
	raw, _, err := s.kv.Get(ctx, secretKey(name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "Secret [%s] not found", name)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading secret: %v", err)
	}

	var secret secretmanagerpb.Secret
	if err := unmarshal.Unmarshal(raw, &secret); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding secret: %v", err)
	}
	return &secret, nil
}

// versions lists a secret's versions, oldest first.
func (s *Service) versions(ctx context.Context, secret string) ([]*secretmanagerpb.SecretVersion, error) {
	entries, _, err := s.kv.List(ctx, versionPrefix(secret), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing versions: %v", err)
	}

	out := make([]*secretmanagerpb.SecretVersion, 0, len(entries))
	for _, kv := range entries {
		var stored storedVersion
		if err := json.Unmarshal(kv.Val, &stored); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding version: %v", err)
		}
		out = append(out, stored.Version)
	}
	return out, nil
}

// Protos are stored as protojson, never encoding/json: a Replication is a
// oneof, which lands in Go as an interface no plain JSON decoder can rebuild.
var (
	marshal   = protojson.MarshalOptions{}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// storedVersion keeps the payload beside the version: a version without its
// value is only metadata, and nothing else holds the bytes.
type storedVersion struct {
	Version *secretmanagerpb.SecretVersion
	Payload []byte
}

// The envelope on disk. The version travels as protojson inside it, so the
// oneof survives; the payload is bytes beside it.
type versionEnvelope struct {
	Version json.RawMessage `json:"version"`
	Payload []byte          `json:"payload"`
}

func (v storedVersion) MarshalJSON() ([]byte, error) {
	encoded, err := marshal.Marshal(v.Version)
	if err != nil {
		return nil, err
	}
	return json.Marshal(versionEnvelope{Version: encoded, Payload: v.Payload})
}

func (v *storedVersion) UnmarshalJSON(data []byte) error {
	var envelope versionEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	v.Payload = envelope.Payload
	v.Version = &secretmanagerpb.SecretVersion{}
	return unmarshal.Unmarshal(envelope.Version, v.Version)
}

// resolve turns a version name — a number, or the latest alias — into the
// stored version.
func (s *Service) resolve(ctx context.Context, secret, version string) (*storedVersion, error) {
	if version == LatestAlias {
		return s.latest(ctx, secret)
	}

	n, err := strconv.ParseInt(version, 10, 64)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid version %q", version)
	}

	raw, _, err := s.kv.Get(ctx, versionKey(secret, n))
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound,
			"Secret Version [%s/versions/%s] not found", secret, version)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading version: %v", err)
	}

	var stored storedVersion
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding version: %v", err)
	}
	return &stored, nil
}

// latest is the newest version that is still enabled.
//
// Not simply the highest number: disabling or destroying the newest version
// has to move the alias, or a rotation that went wrong would keep serving a
// value the operator has just revoked.
func (s *Service) latest(ctx context.Context, secret string) (*storedVersion, error) {
	entries, _, err := s.kv.List(ctx, versionPrefix(secret), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing versions: %v", err)
	}

	for i := len(entries) - 1; i >= 0; i-- {
		var stored storedVersion
		if err := json.Unmarshal(entries[i].Val, &stored); err != nil {
			continue
		}
		if stored.Version.GetState() == secretmanagerpb.SecretVersion_ENABLED {
			return &stored, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "Secret Version [%s/versions/latest] not found", secret)
}
