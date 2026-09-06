// Package cloudlogging is the Cloud Logging emulation.
//
// It exists for the one thing you cannot do against a real project locally:
// assert on what your code logged. A service writes structured entries with
// WriteLogEntries; a test (or gcloud) reads them back with ListLogEntries and
// a filter. "When a payment fails, does an ERROR entry carry the order id?" is
// a real thing to test, and this is where that entry lands.
package cloudlogging

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/monirz/cloudrig/core/clock"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service holds log entries in memory.
//
// In memory rather than the KV store: logs are append-heavy and read by query,
// not by key, and a local run keeps few enough that a bounded slice is simpler
// than a key layout. The bound keeps a chatty service from growing without end.
type Service struct {
	loggingpb.UnimplementedLoggingServiceV2Server

	clk clock.Clock

	mu      sync.Mutex
	entries []*loggingpb.LogEntry // oldest first
	max     int
	seq     uint64 // insert id source, also breaks timestamp ties
}

// MaxEntries is how many entries are kept. Enough to assert on a test's output,
// small enough that a loop logging forever cannot exhaust memory.
const MaxEntries = 10000

var marshal = protojson.MarshalOptions{}

// New wires a service.
func New(clk clock.Clock) *Service {
	return &Service{clk: clk, max: MaxEntries}
}

// WriteLogEntries appends entries, filling the timestamp and id the server
// stamps. A per-request LogName, Resource or Labels is the default for entries
// that omit their own, which is how the high-level client writes a batch.
func (s *Service) WriteLogEntries(ctx context.Context, req *loggingpb.WriteLogEntriesRequest) (*loggingpb.WriteLogEntriesResponse, error) {
	now := timestamppb.New(s.clk.Now())

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range req.GetEntries() {
		if e.GetLogName() == "" {
			e.LogName = req.GetLogName()
		}
		if e.GetResource() == nil {
			e.Resource = req.GetResource()
		}
		if e.GetTimestamp() == nil {
			e.Timestamp = now
		}
		e.ReceiveTimestamp = now
		if e.GetInsertId() == "" {
			e.InsertId = insertID(s.seq)
		}
		// The labels merge: request labels are the base, the entry's own win.
		e.Labels = mergeLabels(req.GetLabels(), e.GetLabels())
		s.seq++
		s.entries = append(s.entries, e)
	}

	// Bounded: drop the oldest once past the cap.
	if len(s.entries) > s.max {
		s.entries = s.entries[len(s.entries)-s.max:]
	}
	return &loggingpb.WriteLogEntriesResponse{}, nil
}

// ListLogEntries returns matching entries. The default order is oldest first;
// "timestamp desc" reverses it, which is what a console tail asks for.
func (s *Service) ListLogEntries(ctx context.Context, req *loggingpb.ListLogEntriesRequest) (*loggingpb.ListLogEntriesResponse, error) {
	pred, err := parseFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	matched := make([]*loggingpb.LogEntry, 0, len(s.entries))
	for _, e := range s.entries {
		if pred(e) {
			matched = append(matched, e)
		}
	}
	s.mu.Unlock()

	if req.GetOrderBy() == "timestamp desc" {
		sort.SliceStable(matched, func(i, j int) bool {
			return matched[i].GetTimestamp().AsTime().After(matched[j].GetTimestamp().AsTime())
		})
	}
	return &loggingpb.ListLogEntriesResponse{Entries: matched}, nil
}

// ListLogs returns the distinct log names that have entries.
func (s *Service) ListLogs(ctx context.Context, req *loggingpb.ListLogsRequest) (*loggingpb.ListLogsResponse, error) {
	s.mu.Lock()
	seen := map[string]bool{}
	for _, e := range s.entries {
		seen[e.GetLogName()] = true
	}
	s.mu.Unlock()

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return &loggingpb.ListLogsResponse{LogNames: names}, nil
}

// DeleteLog drops every entry for one log name.
func (s *Service) DeleteLog(ctx context.Context, req *loggingpb.DeleteLogRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := s.entries[:0]
	for _, e := range s.entries {
		if e.GetLogName() != req.GetLogName() {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	return &emptypb.Empty{}, nil
}

// Reset clears entries for a project, or all of them when project is empty.
func (s *Service) Reset(ctx context.Context, project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if project == "" {
		s.entries = nil
		return nil
	}
	needle := "projects/" + project + "/"
	kept := s.entries[:0]
	for _, e := range s.entries {
		if !hasPrefix(e.GetLogName(), needle) {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	return nil
}

func mergeLabels(base, over map[string]string) map[string]string {
	if len(base) == 0 {
		return over
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func insertID(seq uint64) string {
	b, _ := json.Marshal(seq)
	return "cloudrig-" + string(b)
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
