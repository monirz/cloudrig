package cloudlogging

import (
	"bytes"
	"context"
	"io"
	"sync"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
)

// FunctionLog returns a writer that records each line a function prints as an
// entry shaped the way Cloud Functions (1st gen) writes them, which is what
// gcloud functions logs read filters on. stdout and stderr arrive merged, so
// every line is INFO.
func (s *Service) FunctionLog(project, region, name string) io.Writer {
	return &lineWriter{emit: func(line string) {
		_, _ = s.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{
			LogName: "projects/" + project + "/logs/cloudfunctions.googleapis.com%2Fcloud-functions",
			Resource: &monitoredres.MonitoredResource{Type: "cloud_function", Labels: map[string]string{
				"project_id": project, "region": region, "function_name": name,
			}},
			Entries: []*loggingpb.LogEntry{{
				Severity: ltype.LogSeverity_INFO,
				Payload:  &loggingpb.LogEntry_TextPayload{TextPayload: line},
			}},
		})
	}}
}

// MaxLineBytes caps a line held while waiting for its newline: Cloud Logging's
// entry size limit. Output that never ends a line, like \r progress bars, is
// emitted in pieces this size rather than buffered without end.
const MaxLineBytes = 256 << 10

// lineWriter calls emit once per complete line. stdout and stderr write from
// different goroutines, and a write can end mid-line.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		if line := string(bytes.TrimRight(w.buf[:i], "\r")); line != "" {
			w.emit(line)
		}
		w.buf = w.buf[i+1:]
	}
	for len(w.buf) >= MaxLineBytes {
		w.emit(string(w.buf[:MaxLineBytes]))
		w.buf = w.buf[MaxLineBytes:]
	}
	// Drop the backing array once drained, so one burst of output is not held
	// for the life of the function.
	if len(w.buf) == 0 {
		w.buf = nil
	}
	return len(p), nil
}
