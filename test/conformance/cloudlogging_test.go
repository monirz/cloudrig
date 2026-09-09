package conformance

import (
	"context"
	"testing"
	"time"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/monirz/cloudrig"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func logClient(t *testing.T) (*logging.Client, *cloudrig.Emulator, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()
	c, err := logging.NewClient(ctx,
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("logging.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, emu, ctx
}

const logName = "projects/test-project/logs/app"

func write(t *testing.T, c *logging.Client, ctx context.Context, sev ltype.LogSeverity, text string, labels map[string]string) {
	t.Helper()
	_, err := c.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
		LogName:  logName,
		Resource: &monitoredres.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{{
			Severity:  sev,
			Labels:    labels,
			Timestamp: timestamppb.New(time.Now()),
			Payload:   &loggingpb.LogEntry_TextPayload{TextPayload: text},
		}},
	})
	if err != nil {
		t.Fatalf("WriteLogEntries: %v", err)
	}
}

func list(t *testing.T, c *logging.Client, ctx context.Context, filter string) []*loggingpb.LogEntry {
	t.Helper()
	it := c.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/test-project"},
		Filter:        filter,
	})
	var out []*loggingpb.LogEntry
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLogEntries: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// TestWriteAndReadBack is the whole point: a service writes a log, a test reads
// it back — which cannot be done against a real project locally.
func TestWriteAndReadBack(t *testing.T) {
	c, _, ctx := logClient(t)

	write(t, c, ctx, ltype.LogSeverity_INFO, "started", nil)
	got := list(t, c, ctx, "")
	if len(got) != 1 || got[0].GetTextPayload() != "started" {
		t.Fatalf("entries = %+v", got)
	}
	if got[0].GetLogName() != logName {
		t.Errorf("logName = %q", got[0].GetLogName())
	}
}

// TestSeverityFilter is the filter a test uses to assert an error was logged.
func TestSeverityFilter(t *testing.T) {
	c, _, ctx := logClient(t)

	write(t, c, ctx, ltype.LogSeverity_INFO, "ok", nil)
	write(t, c, ctx, ltype.LogSeverity_ERROR, "boom", nil)
	write(t, c, ctx, ltype.LogSeverity_WARNING, "careful", nil)

	errs := list(t, c, ctx, "severity>=ERROR")
	if len(errs) != 1 || errs[0].GetTextPayload() != "boom" {
		t.Errorf("severity>=ERROR = %+v", errs)
	}
}

// TestLabelFilter covers the structured-log case: asserting an entry carried a
// field.
func TestLabelFilter(t *testing.T) {
	c, _, ctx := logClient(t)

	write(t, c, ctx, ltype.LogSeverity_ERROR, "payment failed", map[string]string{"orderId": "42"})
	write(t, c, ctx, ltype.LogSeverity_ERROR, "other", map[string]string{"orderId": "7"})

	got := list(t, c, ctx, `labels.orderId="42"`)
	if len(got) != 1 || got[0].GetTextPayload() != "payment failed" {
		t.Errorf("labels.orderId filter = %+v", got)
	}
}

// TestListLogsAndDelete covers enumerating log names and clearing one.
func TestListLogsAndDelete(t *testing.T) {
	c, _, ctx := logClient(t)

	write(t, c, ctx, ltype.LogSeverity_INFO, "x", nil)
	if got := list(t, c, ctx, ""); len(got) == 0 {
		t.Fatal("nothing written")
	}
	if err := c.DeleteLog(ctx, &loggingpb.DeleteLogRequest{LogName: logName}); err != nil {
		t.Fatalf("DeleteLog: %v", err)
	}
	if got := list(t, c, ctx, ""); len(got) != 0 {
		t.Errorf("entries survived a delete: %+v", got)
	}
}

// TestUnsupportedFilterIsAnError holds that a filter term the emulator does not
// model fails loudly rather than matching everything and misleading a test.
func TestUnsupportedFilterIsAnError(t *testing.T) {
	c, _, ctx := logClient(t)
	write(t, c, ctx, ltype.LogSeverity_INFO, "x", nil)

	it := c.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/test-project"},
		Filter:        `httpRequest.status=500`,
	})
	_, err := it.Next()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("unsupported filter = %v, want InvalidArgument", err)
	}
}
