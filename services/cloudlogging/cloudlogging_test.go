package cloudlogging

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/monirz/cloudrig/core/clock"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func entry(sev ltype.LogSeverity, labels map[string]string) *loggingpb.LogEntry {
	return &loggingpb.LogEntry{Severity: sev, Labels: labels}
}

func TestParseFilter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		filter string
		entry  *loggingpb.LogEntry
		match  bool
	}{
		{"", entry(ltype.LogSeverity_INFO, nil), true},
		{"severity>=ERROR", entry(ltype.LogSeverity_ERROR, nil), true},
		{"severity>=ERROR", entry(ltype.LogSeverity_WARNING, nil), false},
		{"severity=INFO", entry(ltype.LogSeverity_INFO, nil), true},
		{"severity=INFO", entry(ltype.LogSeverity_ERROR, nil), false},
		{`labels.k="v"`, entry(ltype.LogSeverity_INFO, map[string]string{"k": "v"}), true},
		{`labels.k="v"`, entry(ltype.LogSeverity_INFO, map[string]string{"k": "x"}), false},
		{`severity>=WARNING labels.k="v"`, entry(ltype.LogSeverity_ERROR, map[string]string{"k": "v"}), true},
		{`severity>=WARNING labels.k="v"`, entry(ltype.LogSeverity_INFO, map[string]string{"k": "v"}), false},
	}

	fn := &loggingpb.LogEntry{
		LogName:   "projects/p/logs/cloudfunctions.googleapis.com%2Fcloud-functions",
		Severity:  ltype.LogSeverity_INFO,
		Timestamp: timestamppb.New(epoch),
		Resource: &monitoredres.MonitoredResource{Type: "cloud_function", Labels: map[string]string{
			"function_name": "hello", "region": "us-central1",
		}},
		Labels: map[string]string{"goog-managed-by": "cloudfunctions"},
	}
	// What gcloud functions logs read sends, verbatim.
	gcloudFn := `(resource.type="cloud_function" resource.labels.region="us-central1" logName:"cloud-functions" resource.labels.function_name="hello") OR (resource.type="cloud_run_revision" labels."goog-managed-by"="cloudfunctions" resource.labels.service_name="hello") timestamp>="2025-12-31T00:00:00Z"`
	cases = append(cases, []struct {
		filter string
		entry  *loggingpb.LogEntry
		match  bool
	}{
		{gcloudFn, fn, true},
		{strings.Replace(gcloudFn, `"hello"`, `"other"`, 1), fn, false},
		{strings.Replace(gcloudFn, "2025-12-31", "2026-01-02", 1), fn, false},
		{`timestamp>="2026-01-01T00:00:00Z" AND logName:smoke`, &loggingpb.LogEntry{LogName: "projects/p/logs/smoke-log", Timestamp: timestamppb.New(epoch)}, true},
		{`timestamp<"2026-01-01T00:00:00Z"`, fn, false},
		{`severity=INFO OR severity=ERROR`, entry(ltype.LogSeverity_ERROR, nil), true},
		// OR binds tighter than AND: (DEBUG OR ERROR) AND k=v, not DEBUG OR (...).
		{`severity=DEBUG OR severity=ERROR labels.k="v"`, entry(ltype.LogSeverity_DEBUG, nil), false},
		{`NOT severity=ERROR`, entry(ltype.LogSeverity_ERROR, nil), false},
		{`-labels.k="v"`, entry(ltype.LogSeverity_INFO, map[string]string{"k": "x"}), true},
		{`labels.k!="v"`, entry(ltype.LogSeverity_INFO, map[string]string{"k": "v"}), false},
		{`labels."goog-managed-by"="cloudfunctions"`, fn, true},
	}...)

	for _, c := range cases {
		pred, err := parseFilter(c.filter)
		if err != nil {
			t.Errorf("parseFilter(%q): %v", c.filter, err)
			continue
		}
		if got := pred(c.entry); got != c.match {
			t.Errorf("filter %q on %v = %v, want %v", c.filter, c.entry, got, c.match)
		}
	}
}

// TestUnsupportedFilterFails is the honesty guard: a term the emulator does not
// model is an error, never a silent match-everything.
func TestUnsupportedFilterFails(t *testing.T) {
	t.Parallel()
	for _, f := range []string{"httpRequest.status=500", "nonsense", "protoPayload.foo=1", "(severity=INFO", `labels.k="v`, "timestamp>=yesterday"} {
		if _, err := parseFilter(f); status.Code(err) != codes.InvalidArgument {
			t.Errorf("parseFilter(%q) = %v, want InvalidArgument", f, err)
		}
	}
}

// TestEntriesAreBounded holds that a service logging forever cannot exhaust
// memory: the oldest entries drop past the cap.
func TestEntriesAreBounded(t *testing.T) {
	t.Parallel()

	s := New(clock.NewFake(epoch))
	s.max = 5
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		s.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
			LogName: "projects/p/logs/l",
			Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "x"}}},
		})
	}
	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	if n != 5 {
		t.Errorf("kept %d entries, want the cap of 5", n)
	}
}

// TestQuotedValueWithSpace holds the split bug: a quoted value containing a
// space must stay one term, not fragment into an error.
func TestQuotedValueWithSpace(t *testing.T) {
	t.Parallel()

	pred, err := parseFilter(`labels.message="payment failed"`)
	if err != nil {
		t.Fatalf("parseFilter: %v", err)
	}
	if !pred(entry(ltype.LogSeverity_ERROR, map[string]string{"message": "payment failed"})) {
		t.Error("a quoted value with a space did not match")
	}
	if pred(entry(ltype.LogSeverity_ERROR, map[string]string{"message": "payment"})) {
		t.Error("matched a different value")
	}
}

// TestProjectScope is the shared-emulator case: a query for one project must
// not return another project's entries.
func TestProjectScope(t *testing.T) {
	t.Parallel()

	s := New(clock.NewFake(epoch))
	ctx := context.Background()
	for _, ln := range []string{"projects/a/logs/x", "projects/b/logs/x"} {
		s.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
			LogName: ln,
			Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: ln}}},
		})
	}

	got, err := s.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetEntries()) != 1 || got.GetEntries()[0].GetLogName() != "projects/a/logs/x" {
		t.Errorf("ListLogEntries for project a returned %d entries: %+v", len(got.GetEntries()), got.GetEntries())
	}

	logs, _ := s.ListLogs(ctx, &loggingpb.ListLogsRequest{Parent: "projects/b"})
	if len(logs.GetLogNames()) != 1 || logs.GetLogNames()[0] != "projects/b/logs/x" {
		t.Errorf("ListLogs for project b returned %v", logs.GetLogNames())
	}
}
