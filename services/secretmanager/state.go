package secretmanager

import (
	"context"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Disabling, enabling and destroying are the rotation controls: a version can
// be taken out of service and put back, or removed for good. Only destroy
// discards the value.

func (s *Service) DisableSecretVersion(ctx context.Context, req *secretmanagerpb.DisableSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return s.setState(ctx, req.GetName(), secretmanagerpb.SecretVersion_DISABLED)
}

func (s *Service) EnableSecretVersion(ctx context.Context, req *secretmanagerpb.EnableSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return s.setState(ctx, req.GetName(), secretmanagerpb.SecretVersion_ENABLED)
}

func (s *Service) DestroySecretVersion(ctx context.Context, req *secretmanagerpb.DestroySecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return s.setState(ctx, req.GetName(), secretmanagerpb.SecretVersion_DESTROYED)
}

// setState moves a version between states, dropping the payload when it is
// destroyed: a destroyed version keeps its number and loses its value, which
// is what makes destroy different from disable.
func (s *Service) setState(ctx context.Context, name string, want secretmanagerpb.SecretVersion_State) (*secretmanagerpb.SecretVersion, error) {
	secret, version, err := parseVersion(name)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := s.resolve(ctx, secret, version)
	if err != nil {
		return nil, err
	}
	// A destroyed version cannot come back; anything else would let a revoked
	// credential be restored by a second call.
	if stored.Version.GetState() == secretmanagerpb.SecretVersion_DESTROYED {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Secret Version [%s] is destroyed", stored.Version.GetName())
	}

	stored.Version.State = want
	if want == secretmanagerpb.SecretVersion_DESTROYED {
		stored.Version.DestroyTime = timestamppb.New(s.clk.Now())
		stored.Payload = nil
	}

	n, err := versionNumber(stored.Version.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.replaceVersion(ctx, secret, n, stored); err != nil {
		return nil, err
	}
	return stored.Version, nil
}
