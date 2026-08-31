package pubsub

import (
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func mask(paths ...string) *fieldmaskpb.FieldMask {
	return &fieldmaskpb.FieldMask{Paths: paths}
}

// TestApplyMaskAcceptsBothSpellings is the bug Terraform found: the REST
// surface sends camelCase mask paths, gRPC sends the proto name, and both name
// the same field.
func TestApplyMaskAcceptsBothSpellings(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"ackDeadlineSeconds", "ack_deadline_seconds"} {
		t.Run(path, func(t *testing.T) {
			dst := &pubsubpb.Subscription{Name: "s", AckDeadlineSeconds: 10}
			src := &pubsubpb.Subscription{AckDeadlineSeconds: 60}

			if err := applyMask(dst, src, mask(path)); err != nil {
				t.Fatalf("applyMask: %v", err)
			}
			if dst.GetAckDeadlineSeconds() != 60 {
				t.Errorf("deadline = %d, want 60", dst.GetAckDeadlineSeconds())
			}
		})
	}
}

// TestApplyMaskTouchesOnlyNamedFields is the point of a mask: an unmentioned
// field survives, even though the request carried a zero value for it.
func TestApplyMaskTouchesOnlyNamedFields(t *testing.T) {
	t.Parallel()

	dst := &pubsubpb.Subscription{
		Name:               "s",
		Topic:              "t",
		AckDeadlineSeconds: 10,
		Labels:             map[string]string{"env": "local"},
	}
	src := &pubsubpb.Subscription{AckDeadlineSeconds: 60}

	if err := applyMask(dst, src, mask("ackDeadlineSeconds")); err != nil {
		t.Fatal(err)
	}
	if dst.GetLabels()["env"] != "local" {
		t.Errorf("labels were overwritten: %v", dst.GetLabels())
	}
	if dst.GetTopic() != "t" {
		t.Errorf("topic = %q, want it untouched", dst.GetTopic())
	}
}

// TestApplyMaskClearsAnOmittedValue covers the other half: naming a field with
// no value in the request means remove it.
func TestApplyMaskClearsAnOmittedValue(t *testing.T) {
	t.Parallel()

	dst := &pubsubpb.Topic{Name: "t", Labels: map[string]string{"env": "local"}}
	if err := applyMask(dst, &pubsubpb.Topic{}, mask("labels")); err != nil {
		t.Fatal(err)
	}
	if len(dst.GetLabels()) != 0 {
		t.Errorf("labels = %v, want cleared", dst.GetLabels())
	}
}

func TestApplyMaskRejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		mask      *fieldmaskpb.FieldMask
		immutable []string
	}{
		{"an empty mask", mask(), nil},
		{"no mask at all", nil, nil},
		{"an unknown field", mask("nonsense"), nil},
		{"an immutable field", mask("topic"), []string{"topic"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := applyMask(&pubsubpb.Subscription{}, &pubsubpb.Subscription{}, c.mask, c.immutable...)
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("err = %v, want InvalidArgument", err)
			}
		})
	}
}
