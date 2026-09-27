package cloudlogging

import (
	"context"
	"io"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/monirz/cloudrig/core/clock"
)

// TestFunctionLogIsWhatGcloudReads: one entry per line, even when a write ends
// mid-line, found by the filter gcloud functions logs read sends.
func TestFunctionLogIsWhatGcloudReads(t *testing.T) {
	t.Parallel()
	s := New(clock.NewFake(epoch))
	w := s.FunctionLog("p", "us-central1", "hello")

	io.WriteString(w, "first\nsec")
	io.WriteString(w, "ond\n\n")
	io.WriteString(s.FunctionLog("p", "us-central1", "other"), "not mine\n")

	out, err := s.ListLogEntries(context.Background(), &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/p"},
		Filter:        `(resource.type="cloud_function" resource.labels.region="us-central1" logName:"cloud-functions" resource.labels.function_name="hello") timestamp>="2025-12-31T00:00:00Z"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range out.GetEntries() {
		got = append(got, e.GetTextPayload())
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("entries = %q, want [first second]", got)
	}
}

// TestFunctionLogBoundsAnUnendedLine: output that never writes a newline is
// emitted in capped pieces, not held in memory without end.
func TestFunctionLogBoundsAnUnendedLine(t *testing.T) {
	t.Parallel()
	var got []string
	w := &lineWriter{emit: func(line string) { got = append(got, line) }}

	chunk := strings.Repeat("x", 64<<10)
	for range 10 { // 640 KiB, no newline
		io.WriteString(w, chunk)
	}
	if len(got) != 2 || len(got[0]) != MaxLineBytes || len(got[1]) != MaxLineBytes {
		t.Fatalf("emitted %d pieces, want 2 of %d bytes", len(got), MaxLineBytes)
	}
	if len(w.buf) != 640<<10-2*MaxLineBytes {
		t.Errorf("held %d bytes, want the %d-byte remainder", len(w.buf), 640<<10-2*MaxLineBytes)
	}

	io.WriteString(w, "\n")
	if len(got) != 3 || len(w.buf) != 0 {
		t.Errorf("the newline did not flush the remainder: %d pieces, %d held", len(got), len(w.buf))
	}
}
