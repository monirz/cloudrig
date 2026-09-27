package cloudlogging

import (
	"context"
	"io"
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
