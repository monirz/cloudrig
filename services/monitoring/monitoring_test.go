package monitoring_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// interruptingStore lets a test interrupt a write between its point and its
// marker: once armed, it runs onPoint after a point is stored, and fails the
// next marker write if failMarker is set.
type interruptingStore struct {
	store.Store
	onPoint    func()
	failMarker bool
}

func (s *interruptingStore) Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error) {
	if strings.HasPrefix(key, "mon/latest/") && s.failMarker {
		s.failMarker = false
		return 0, errors.New("disk full")
	}
	v, err := s.Store.Put(ctx, key, val, ifVersion)
	if strings.HasPrefix(key, "mon/ts/") && s.onPoint != nil {
		s.onPoint()
		s.onPoint = nil
	}
	return v, err
}

// A write interrupted after its point is stored must not leave the series'
// marker behind that point, or an older point would be let in later.
func TestInterruptedWriteKeepsPointOrder(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	point := func(sec int) *monitoringpb.CreateTimeSeriesRequest {
		return &monitoringpb.CreateTimeSeriesRequest{
			Name: "projects/p",
			TimeSeries: []*monitoringpb.TimeSeries{{
				Metric:   &metric.Metric{Type: "custom.googleapis.com/n"},
				Resource: &monitoredres.MonitoredResource{Type: "global"},
				Points: []*monitoringpb.Point{{
					Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(t0.Add(time.Duration(sec) * time.Second))},
					Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: int64(sec)}},
				}},
			}},
		}
	}
	for name, interrupt := range map[string]func(kv *interruptingStore, cancel context.CancelFunc){
		"caller cancels": func(kv *interruptingStore, cancel context.CancelFunc) { kv.onPoint = cancel },
		"marker fails":   func(kv *interruptingStore, _ context.CancelFunc) { kv.failMarker = true },
	} {
		t.Run(name, func(t *testing.T) {
			kv := &interruptingStore{Store: store.NewMemory()}
			svc := monitoring.New(kv, clock.NewFake(time.Unix(0, 0)))
			if _, err := svc.CreateTimeSeries(context.Background(), point(10)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			interrupt(kv, cancel)
			_, _ = svc.CreateTimeSeries(ctx, point(20))
			cancel()

			_, err := svc.CreateTimeSeries(context.Background(), point(15))
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("a point older than one stored = %v, want InvalidArgument", err)
			}
		})
	}
}
