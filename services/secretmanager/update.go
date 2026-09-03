package secretmanager

import (
	"context"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// UpdateSecret applies the fields named by the update mask. `gcloud secrets
// update` reaches this to change labels.
func (s *Service) UpdateSecret(ctx context.Context, req *secretmanagerpb.UpdateSecretRequest) (*secretmanagerpb.Secret, error) {
	name := req.GetSecret().GetName()
	if _, _, err := parseSecret(name); err != nil {
		return nil, err
	}

	raw, version, err := s.kv.Get(ctx, secretKey(name))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Secret [%s] not found", name)
	}
	var current secretmanagerpb.Secret
	if err := unmarshal.Unmarshal(raw, &current); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding secret: %v", err)
	}

	if err := applyMask(&current, req.GetSecret(), req.GetUpdateMask()); err != nil {
		return nil, err
	}

	encoded, err := marshal.Marshal(&current)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding secret: %v", err)
	}
	// At the version just read, so a concurrent update cannot be lost.
	if _, err := s.kv.Put(ctx, secretKey(name), encoded, version); err != nil {
		return nil, status.Errorf(codes.Aborted, "updating secret: %v", err)
	}
	return &current, nil
}

// applyMask copies the fields named by mask from src onto dst.
//
// Only the named fields move: a masked update carrying the whole resource
// would overwrite fields the caller never mentioned, which is the bug an
// update mask exists to prevent. The name is fixed at creation.
func applyMask(dst, src *secretmanagerpb.Secret, mask *fieldmaskpb.FieldMask) error {
	paths := mask.GetPaths()
	if len(paths) == 0 {
		return status.Error(codes.InvalidArgument, "an update needs a non-empty update_mask")
	}

	d, s := dst.ProtoReflect(), src.ProtoReflect()
	fields := d.Descriptor().Fields()

	for _, path := range paths {
		if path == "name" {
			return status.Error(codes.InvalidArgument, "field \"name\" cannot be updated")
		}

		// A mask arrives in whichever spelling the caller's surface uses: REST
		// sends camelCase, gRPC sends the proto name.
		fd := fields.ByName(protoreflect.Name(path))
		if fd == nil {
			fd = fields.ByJSONName(path)
		}
		if fd == nil {
			return status.Errorf(codes.InvalidArgument, "unknown field %q in update_mask", path)
		}

		// An unset field in the request clears the stored one: that is what
		// naming it in the mask without a value means.
		if s.Has(fd) {
			d.Set(fd, s.Get(fd))
		} else {
			d.Clear(fd)
		}
	}
	return nil
}
