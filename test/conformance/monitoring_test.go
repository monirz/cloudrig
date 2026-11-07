package conformance

import (
	"context"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/monirz/cloudrig"
)

func monitoringClient(t *testing.T) (*monitoring.MetricClient, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()
	c, err := monitoring.NewMetricClient(ctx,
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("monitoring.NewMetricClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

const filesMetric = "custom.googleapis.com/pipeline/files_done"

// gauge is one point of the files-done metric for a step.
func gauge(step string, at time.Time, v int64) *monitoringpb.TimeSeries {
	return &monitoringpb.TimeSeries{
		Metric:   &metric.Metric{Type: filesMetric, Labels: map[string]string{"step": step}},
		Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": "p"}},
		Points: []*monitoringpb.Point{{
			Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(at)},
			Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}},
		}},
	}
}

func listSeries(t *testing.T, c *monitoring.MetricClient, ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) []*monitoringpb.TimeSeries {
	t.Helper()
	var out []*monitoringpb.TimeSeries
	it := c.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("ListTimeSeries: %v", err)
		}
		out = append(out, ts)
	}
}

func TestMonitoringWriteThenList(t *testing.T) {
	c, ctx := monitoringClient(t)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	for i, batch := range [][]*monitoringpb.TimeSeries{
		{gauge("ocr", t0, 10), gauge("redact", t0, 4)},
		{gauge("ocr", t0.Add(time.Minute), 25)},
		{gauge("ocr", t0.Add(2*time.Minute), 40)},
	} {
		if err := c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/p", TimeSeries: batch}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	window := &monitoringpb.TimeInterval{StartTime: timestamppb.New(t0.Add(-time.Second)), EndTime: timestamppb.New(t0.Add(time.Hour))}
	all := listSeries(t, c, ctx, &monitoringpb.ListTimeSeriesRequest{
		Name: "projects/p", Filter: `metric.type = "` + filesMetric + `"`, Interval: window,
	})
	if len(all) != 2 {
		t.Fatalf("series = %d, want 2 (one per step)", len(all))
	}

	ocr := listSeries(t, c, ctx, &monitoringpb.ListTimeSeriesRequest{
		Name:     "projects/p",
		Filter:   `metric.type = "` + filesMetric + `" AND metric.labels.step = "ocr"`,
		Interval: window,
	})
	if len(ocr) != 1 {
		t.Fatalf("ocr series = %d, want 1", len(ocr))
	}
	var got []int64
	for _, p := range ocr[0].GetPoints() {
		got = append(got, p.GetValue().GetInt64Value())
	}
	// Newest first, as the real API answers.
	if len(got) != 3 || got[0] != 40 || got[2] != 10 {
		t.Errorf("ocr points = %v, want [40 25 10]", got)
	}
	if ocr[0].GetMetricKind() != metric.MetricDescriptor_GAUGE || ocr[0].GetValueType() != metric.MetricDescriptor_INT64 {
		t.Errorf("kind %s, type %s; want GAUGE INT64", ocr[0].GetMetricKind(), ocr[0].GetValueType())
	}

	// The window is (start, end]: a start at the second point leaves it out.
	late := listSeries(t, c, ctx, &monitoringpb.ListTimeSeriesRequest{
		Name:     "projects/p",
		Filter:   `metric.type = "` + filesMetric + `" AND metric.labels.step = "ocr"`,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(t0.Add(time.Minute)), EndTime: timestamppb.New(t0.Add(time.Hour))},
	})
	if len(late) != 1 || len(late[0].GetPoints()) != 1 {
		t.Errorf("late window = %v, want the one newest point", late)
	}
}

func TestMonitoringRefusals(t *testing.T) {
	c, ctx := monitoringClient(t)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{
		Name: "projects/p", TimeSeries: []*monitoringpb.TimeSeries{gauge("ocr", t0, 1)},
	}); err != nil {
		t.Fatal(err)
	}
	two := gauge("x", t0, 1)
	two.Points = append(two.Points, two.Points[0])

	window := &monitoringpb.TimeInterval{EndTime: timestamppb.New(t0)}
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"an out-of-order point", c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{
			Name: "projects/p", TimeSeries: []*monitoringpb.TimeSeries{gauge("ocr", t0.Add(-time.Minute), 1)},
		}), codes.InvalidArgument},
		{"two points in one series", c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{
			Name: "projects/p", TimeSeries: []*monitoringpb.TimeSeries{two},
		}), codes.InvalidArgument},
		{"a filter with no metric type", func() error {
			_, err := c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
				Name: "projects/p", Filter: `resource.type = "global"`, Interval: window,
			}).Next()
			return err
		}(), codes.InvalidArgument},
		{"a filter cloudrig cannot read", func() error {
			_, err := c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
				Name: "projects/p", Filter: `metric.type = starts_with("custom")`, Interval: window,
			}).Next()
			return err
		}(), codes.Unimplemented},
		{"aggregation", func() error {
			_, err := c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
				Name: "projects/p", Filter: `metric.type = "` + filesMetric + `"`, Interval: window,
				Aggregation: &monitoringpb.Aggregation{PerSeriesAligner: monitoringpb.Aggregation_ALIGN_MEAN},
			}).Next()
			return err
		}(), codes.Unimplemented},
	}
	for _, tc := range cases {
		if got := status.Code(tc.err); got != tc.want {
			t.Errorf("%s: code = %s, want %s (%v)", tc.name, got, tc.want, tc.err)
		}
	}
}
