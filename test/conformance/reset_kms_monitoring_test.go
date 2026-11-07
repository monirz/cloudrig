package conformance

import (
	"context"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/monirz/cloudrig"
)

// TestResetClearsKMSAndMonitoring holds that a project reset clears KMS key
// rings and Monitoring points, which live outside the storage tree, and
// leaves other projects' alone.
func TestResetClearsKMSAndMonitoring(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()
	opts := []option.ClientOption{
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}
	kc, err := kms.NewKeyManagementClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	mc, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mc.Close() })

	ring := func(project string) error {
		_, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{
			Parent: "projects/" + project + "/locations/global", KeyRingId: "ring",
		})
		return err
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	write := func(project string, at time.Time) error {
		ts := gauge("resize", at, 1)
		ts.Resource.Labels["project_id"] = project
		return mc.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{
			Name: "projects/" + project, TimeSeries: []*monitoringpb.TimeSeries{ts},
		})
	}
	count := func(project string) int {
		return len(listSeries(t, mc, ctx, &monitoringpb.ListTimeSeriesRequest{
			Name:   "projects/" + project,
			Filter: `metric.type = "` + filesMetric + `"`,
			Interval: &monitoringpb.TimeInterval{
				StartTime: timestamppb.New(t0.Add(-time.Hour)), EndTime: timestamppb.New(t0.Add(time.Hour)),
			},
		}))
	}

	for _, p := range []string{"p", "q"} {
		if err := ring(p); err != nil {
			t.Fatalf("ring in %s: %v", p, err)
		}
		if err := write(p, t0); err != nil {
			t.Fatalf("point in %s: %v", p, err)
		}
	}

	if err := emu.Reset(ctx, "p"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if err := ring("p"); err != nil {
		t.Errorf("re-creating p's ring after reset: %v", err)
	}
	if n := count("p"); n != 0 {
		t.Errorf("p has %d series after reset, want 0", n)
	}
	// An earlier point is accepted: p's latest-time marker went too.
	if err := write("p", t0.Add(-time.Minute)); err != nil {
		t.Errorf("writing an earlier point after reset: %v", err)
	}

	if err := ring("q"); err == nil {
		t.Error("q's ring was cleared by p's reset")
	}
	if n := count("q"); n != 1 {
		t.Errorf("q has %d series after p's reset, want 1", n)
	}
}
