package monitoring_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/services/monitoring"
	"github.com/monirz/cloudrig/store"
)

// countingStore counts List calls, the scan a write must not repeat.
type countingStore struct {
	store.Store
	lists atomic.Int64
}

func (c *countingStore) List(ctx context.Context, prefix string, limit int, token string) ([]store.KV, string, error) {
	c.lists.Add(1)
	return c.Store.List(ctx, prefix, limit, token)
}

// A series' writes must not each rescan its earlier points: that made a
// series cost O(n^2) over its life. At most the first write scans.
func TestWritesDoNotRescanTheSeries(t *testing.T) {
	kv := &countingStore{Store: store.NewMemory()}
	svc := monitoring.New(kv, clock.NewFake(time.Unix(0, 0)))
	ctx := context.Background()
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	const writes = 50
	for i := range writes {
		_, err := svc.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{
			Name: "projects/p",
			TimeSeries: []*monitoringpb.TimeSeries{{
				Metric:   &metric.Metric{Type: "custom.googleapis.com/n"},
				Resource: &monitoredres.MonitoredResource{Type: "global"},
				Points: []*monitoringpb.Point{{
					Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(t0.Add(time.Duration(i) * time.Second))},
					Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: int64(i)}},
				}},
			}},
		})
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if n := kv.lists.Load(); n > 1 {
		t.Errorf("%d writes scanned the series %d times, want at most once", writes, n)
	}
}
