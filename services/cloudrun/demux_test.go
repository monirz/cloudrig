package cloudrun

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// frame builds one of Docker's multiplexed chunks: an eight-byte header whose
// last four bytes are the payload length, big-endian.
func frame(stream byte, payload string) []byte {
	n := len(payload)
	h := []byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	return append(h, payload...)
}

func TestDemultiplexStripsTheFraming(t *testing.T) {
	t.Parallel()

	var in bytes.Buffer
	in.Write(frame(1, "out one\n"))
	in.Write(frame(2, "err one\n"))
	in.Write(frame(1, "out two\n"))

	var got bytes.Buffer
	demultiplex(&in, &got)

	want := "out one\nerr one\nout two\n"
	if got.String() != want {
		t.Errorf("demultiplex = %q, want %q", got.String(), want)
	}
}

// A stream that stops mid-frame is what a killed container looks like: the
// output so far is kept, and the loop ends rather than spinning.
func TestDemultiplexStopsOnATruncatedStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"a partial header", []byte{1, 0, 0}, ""},
		{"a header with no payload", frame(1, "hello")[:8], ""},
		{"a payload cut short", frame(1, "hello")[:11], "hel"},
		{"a good frame then a partial one", append(frame(1, "ok\n"), 1, 0, 0), "ok\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got bytes.Buffer
			demultiplex(bytes.NewReader(tc.in), &got)
			if got.String() != tc.want {
				t.Errorf("demultiplex = %q, want %q", got.String(), tc.want)
			}
		})
	}
}

// A zero-length frame is legal and carries nothing; the next frame still
// reads.
func TestDemultiplexHandlesAnEmptyFrame(t *testing.T) {
	t.Parallel()

	var in bytes.Buffer
	in.Write(frame(1, ""))
	in.Write(frame(1, "after\n"))

	var got bytes.Buffer
	demultiplex(&in, &got)

	if got.String() != "after\n" {
		t.Errorf("demultiplex = %q, want %q", got.String(), "after\n")
	}
}

// A payload larger than one read still arrives whole: the copy is by length,
// not by buffer.
func TestDemultiplexHandlesALargePayload(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("x", 128*1024)
	var got bytes.Buffer
	demultiplex(bytes.NewReader(frame(1, payload)), &got)

	if got.Len() != len(payload) {
		t.Errorf("wrote %d bytes, want %d", got.Len(), len(payload))
	}
}

// errAfter yields n bytes and then fails, standing in for a connection that
// drops mid-stream.
type errAfter struct {
	data []byte
	n    int
}

func (e *errAfter) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, e.data[:min(len(p), e.n)])
	e.data = e.data[n:]
	e.n -= n
	return n, nil
}

// A read that fails partway leaves the writer with what did arrive.
func TestDemultiplexStopsOnAReadFailure(t *testing.T) {
	t.Parallel()

	full := frame(1, "abcdefgh")
	var got bytes.Buffer
	demultiplex(&errAfter{data: full, n: 12}, &got)

	if got.String() != "abcd" {
		t.Errorf("demultiplex = %q, want %q", got.String(), "abcd")
	}
}
