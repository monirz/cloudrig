// Package monitoring is the Cloud Monitoring emulation, for time-series
// writes: CreateTimeSeries stores each point, and ListTimeSeries reads them
// back by filter and interval, so a test can assert on what code reported.
//
// Aggregation, alerting, dashboards and metric descriptors are not here. The
// methods for them answer Unimplemented.
package monitoring

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

// Service stores time-series points.
type Service struct {
	monitoringpb.UnimplementedMetricServiceServer

	kv  store.Store
	clk clock.Clock

	// mu makes each write's order check and its store one step. Without it two
	// writes to a series could both pass the check, storing an older point
	// after a newer one, or failing a duplicate as Internal.
	mu sync.Mutex
}

// New wires a service.
func New(kv store.Store, clk clock.Clock) *Service {
	return &Service{kv: kv, clk: clk}
}

// The real API accepts at most 200 series per call.
const maxSeriesPerCall = 200

var (
	marshal   = protojson.MarshalOptions{}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// Key layout: mon/ts/{project}/{metric type}\x00{series id}/{end time}. The
// NUL ends the metric type, since a type such as custom.googleapis.com/a/b
// holds slashes. The end time is fixed-width, so key order is time order
// within a series.
func seriesPrefix(project, metricType string) string {
	return "mon/ts/" + project + "/" + metricType + "\x00"
}

func pointKey(project, metricType, series string, end time.Time) string {
	return fmt.Sprintf("%s%s/%020d", seriesPrefix(project, metricType), series, end.UnixNano())
}

// seriesID names one series: a metric type with one set of metric labels, on
// one monitored resource with one set of resource labels.
func seriesID(ts *monitoringpb.TimeSeries) string {
	ident := &monitoringpb.TimeSeries{Metric: ts.GetMetric(), Resource: ts.GetResource()}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(ident)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

func parseProject(name string) (string, error) {
	project, ok := strings.CutPrefix(name, "projects/")
	if !ok || project == "" || strings.Contains(project, "/") {
		return "", status.Errorf(codes.InvalidArgument, "invalid name %q; expected projects/{project}", name)
	}
	return project, nil
}

func (s *Service) CreateTimeSeries(ctx context.Context, req *monitoringpb.CreateTimeSeriesRequest) (*emptypb.Empty, error) {
	return s.write(ctx, req)
}

// CreateServiceTimeSeries is the same write, used by Google services for
// their own metrics.
func (s *Service) CreateServiceTimeSeries(ctx context.Context, req *monitoringpb.CreateTimeSeriesRequest) (*emptypb.Empty, error) {
	return s.write(ctx, req)
}

func (s *Service) write(ctx context.Context, req *monitoringpb.CreateTimeSeriesRequest) (*emptypb.Empty, error) {
	project, err := parseProject(req.GetName())
	if err != nil {
		return nil, err
	}
	series := req.GetTimeSeries()
	if len(series) == 0 {
		return nil, status.Error(codes.InvalidArgument, "time_series must not be empty")
	}
	if len(series) > maxSeriesPerCall {
		return nil, status.Errorf(codes.InvalidArgument, "at most %d time series per request, got %d",
			maxSeriesPerCall, len(series))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Checked whole before anything is stored, so a bad request leaves no
	// partial write behind.
	ids := make([]string, len(series))
	seen := map[string]bool{}
	for i, ts := range series {
		if err := validSeries(i, ts); err != nil {
			return nil, err
		}
		ids[i] = seriesID(ts)
		if seen[ids[i]] {
			return nil, status.Errorf(codes.InvalidArgument,
				"time_series[%d] repeats a series already in this request", i)
		}
		seen[ids[i]] = true
		latest, err := s.latest(ctx, project, ts.GetMetric().GetType(), ids[i])
		if err != nil {
			return nil, err
		}
		end := ts.GetPoints()[0].GetInterval().GetEndTime().AsTime()
		if !latest.IsZero() && !end.After(latest) {
			return nil, status.Errorf(codes.InvalidArgument,
				"time_series[%d]: Points must be written in order. One or more of the points specified "+
					"had an older end time than the most recent point.", i)
		}
	}

	for i, ts := range series {
		stored := proto.Clone(ts).(*monitoringpb.TimeSeries)
		if stored.GetMetricKind() == metric.MetricDescriptor_METRIC_KIND_UNSPECIFIED {
			stored.MetricKind = metric.MetricDescriptor_GAUGE
		}
		if stored.GetValueType() == metric.MetricDescriptor_VALUE_TYPE_UNSPECIFIED {
			stored.ValueType = valueType(stored.GetPoints()[0].GetValue())
		}
		encoded, err := marshal.Marshal(stored)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encoding time series: %v", err)
		}
		end := ts.GetPoints()[0].GetInterval().GetEndTime().AsTime()
		if _, err := s.kv.Put(ctx, pointKey(project, ts.GetMetric().GetType(), ids[i], end), encoded, 0); err != nil {
			return nil, status.Errorf(codes.Internal, "storing time series: %v", err)
		}
	}
	return &emptypb.Empty{}, nil
}

// validSeries applies the rules the real API enforces on a write.
func validSeries(i int, ts *monitoringpb.TimeSeries) error {
	bad := func(format string, a ...any) error {
		return status.Errorf(codes.InvalidArgument, "time_series[%d]: "+format, append([]any{i}, a...)...)
	}
	switch {
	case ts.GetMetric().GetType() == "":
		return bad("metric.type is required")
	case ts.GetResource().GetType() == "":
		return bad("resource.type is required")
	case len(ts.GetPoints()) != 1:
		return bad("exactly one point is required, got %d", len(ts.GetPoints()))
	}
	p := ts.GetPoints()[0]
	if p.GetInterval().GetEndTime() == nil {
		return bad("the point needs an interval.end_time")
	}
	if p.GetValue().GetValue() == nil {
		return bad("the point needs a value")
	}
	start, end := p.GetInterval().GetStartTime(), p.GetInterval().GetEndTime()
	switch ts.GetMetricKind() {
	case metric.MetricDescriptor_CUMULATIVE, metric.MetricDescriptor_DELTA:
		if start == nil || !start.AsTime().Before(end.AsTime()) {
			return bad("a %s point needs a start_time before its end_time", ts.GetMetricKind())
		}
	default:
		if start != nil && !start.AsTime().Equal(end.AsTime()) {
			return bad("a GAUGE point's start_time, if given, must equal its end_time")
		}
	}
	return nil
}

func valueType(v *monitoringpb.TypedValue) metric.MetricDescriptor_ValueType {
	switch v.GetValue().(type) {
	case *monitoringpb.TypedValue_BoolValue:
		return metric.MetricDescriptor_BOOL
	case *monitoringpb.TypedValue_Int64Value:
		return metric.MetricDescriptor_INT64
	case *monitoringpb.TypedValue_DoubleValue:
		return metric.MetricDescriptor_DOUBLE
	case *monitoringpb.TypedValue_StringValue:
		return metric.MetricDescriptor_STRING
	case *monitoringpb.TypedValue_DistributionValue:
		return metric.MetricDescriptor_DISTRIBUTION
	}
	return metric.MetricDescriptor_VALUE_TYPE_UNSPECIFIED
}

// latest is the end time of a series' newest point, or zero if it has none.
func (s *Service) latest(ctx context.Context, project, metricType, series string) (time.Time, error) {
	entries, _, err := s.kv.List(ctx, seriesPrefix(project, metricType)+series+"/", 0, "")
	if err != nil {
		return time.Time{}, status.Errorf(codes.Internal, "reading time series: %v", err)
	}
	if len(entries) == 0 {
		return time.Time{}, nil
	}
	var ts monitoringpb.TimeSeries
	if err := unmarshal.Unmarshal(entries[len(entries)-1].Val, &ts); err != nil {
		return time.Time{}, status.Errorf(codes.Internal, "decoding time series: %v", err)
	}
	return ts.GetPoints()[0].GetInterval().GetEndTime().AsTime(), nil
}

func (s *Service) ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
	project, err := parseProject(req.GetName())
	if err != nil {
		return nil, err
	}
	switch {
	case req.GetAggregation() != nil || req.GetSecondaryAggregation() != nil:
		return nil, status.Error(codes.Unimplemented, "aggregation is not supported by cloudrig")
	case req.GetOrderBy() != "":
		return nil, status.Error(codes.Unimplemented, "orderBy is not supported by cloudrig")
	case req.GetPageToken() != "":
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	f, err := parseFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	end := req.GetInterval().GetEndTime()
	if end == nil {
		return nil, status.Error(codes.InvalidArgument, "interval.end_time is required")
	}
	// The interval is (start, end]; a missing start means the instant at end.
	from, to := end.AsTime(), end.AsTime()
	if start := req.GetInterval().GetStartTime(); start != nil {
		from = start.AsTime()
		if from.After(to) {
			return nil, status.Error(codes.InvalidArgument, "interval.start_time is after interval.end_time")
		}
	}

	entries, _, err := s.kv.List(ctx, seriesPrefix(project, f.metricType), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing time series: %v", err)
	}

	// Points arrive grouped by series in time order; each series collects its
	// points in the window.
	bySeries := map[string]*monitoringpb.TimeSeries{}
	var order []string
	for _, e := range entries {
		var ts monitoringpb.TimeSeries
		if err := unmarshal.Unmarshal(e.Val, &ts); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding time series: %v", err)
		}
		if !f.matches(&ts) {
			continue
		}
		p := ts.GetPoints()[0]
		at := p.GetInterval().GetEndTime().AsTime()
		inWindow := at.Equal(to) || at.After(from) && at.Before(to)
		if !inWindow {
			continue
		}
		id := seriesID(&ts)
		out, ok := bySeries[id]
		if !ok {
			out = &ts
			out.Points = nil
			bySeries[id] = out
			order = append(order, id)
		}
		out.Points = append(out.Points, p)
	}

	resp := &monitoringpb.ListTimeSeriesResponse{}
	for _, id := range order {
		ts := bySeries[id]
		// Newest first, as the real API answers.
		sort.SliceStable(ts.Points, func(i, j int) bool {
			return ts.Points[i].GetInterval().GetEndTime().AsTime().After(ts.Points[j].GetInterval().GetEndTime().AsTime())
		})
		if req.GetView() == monitoringpb.ListTimeSeriesRequest_HEADERS {
			ts.Points = nil
		}
		resp.TimeSeries = append(resp.TimeSeries, ts)
	}
	return resp, nil
}

// filter is the subset of the monitoring filter language cloudrig reads:
// equalities joined by AND, with metric.type required.
type filter struct {
	metricType     string
	resourceType   string
	metricLabels   map[string]string
	resourceLabels map[string]string
}

var clause = regexp.MustCompile(`^\s*([a-z_.]+[A-Za-z0-9_]*)\s*=\s*"((?:[^"\\]|\\.)*)"\s*$`)

func parseFilter(raw string) (filter, error) {
	f := filter{metricLabels: map[string]string{}, resourceLabels: map[string]string{}}
	if strings.TrimSpace(raw) == "" {
		return f, status.Error(codes.InvalidArgument, `filter is required, and must name a metric.type`)
	}
	unsupported := func() (filter, error) {
		return filter{}, status.Errorf(codes.Unimplemented,
			`cloudrig reads only filters of field = "value" joined by AND, on metric.type, resource.type, `+
				`metric.labels.* and resource.labels.*: %q`, raw)
	}
	for _, part := range regexp.MustCompile(`\s+AND\s+`).Split(raw, -1) {
		m := clause.FindStringSubmatch(part)
		if m == nil {
			return unsupported()
		}
		field, value := m[1], strings.ReplaceAll(m[2], `\"`, `"`)
		switch {
		case field == "metric.type":
			f.metricType = value
		case field == "resource.type":
			f.resourceType = value
		case strings.HasPrefix(field, "metric.labels."):
			f.metricLabels[strings.TrimPrefix(field, "metric.labels.")] = value
		case strings.HasPrefix(field, "resource.labels."):
			f.resourceLabels[strings.TrimPrefix(field, "resource.labels.")] = value
		default:
			return unsupported()
		}
	}
	if f.metricType == "" {
		return filter{}, status.Error(codes.InvalidArgument, `the filter must name a metric.type`)
	}
	return f, nil
}

func (f filter) matches(ts *monitoringpb.TimeSeries) bool {
	if f.resourceType != "" && ts.GetResource().GetType() != f.resourceType {
		return false
	}
	for k, v := range f.metricLabels {
		if ts.GetMetric().GetLabels()[k] != v {
			return false
		}
	}
	for k, v := range f.resourceLabels {
		if ts.GetResource().GetLabels()[k] != v {
			return false
		}
	}
	return true
}
