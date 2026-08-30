package functions

import (
	"strings"
	"sync"
	"testing"
)

func TestLogRingSplitsLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		writes []string
		want   string
	}{
		{"one write per line", []string{"a\n", "b\n"}, "a,b"},
		{"several lines in one write", []string{"a\nb\nc\n"}, "a,b,c"},
		{"a line split across writes", []string{"he", "llo\n"}, "hello"},
		{"an unterminated line is held back", []string{"a\n", "partial"}, "a"},
		{"carriage returns are trimmed", []string{"a\r\n"}, "a"},
		{"an empty line is kept", []string{"\n"}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ring := newLogRing(100)
			for _, w := range tc.writes {
				if _, err := ring.Write([]byte(w)); err != nil {
					t.Fatal(err)
				}
			}
			if got := strings.Join(ring.Snapshot(), ","); got != tc.want {
				t.Errorf("lines = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLogRingIsBounded(t *testing.T) {
	t.Parallel()

	// A chatty function must not grow the emulator without bound.
	ring := newLogRing(3)
	for _, line := range []string{"1", "2", "3", "4", "5"} {
		ring.Write([]byte(line + "\n"))
	}
	if got := strings.Join(ring.Snapshot(), ","); got != "3,4,5" {
		t.Errorf("lines = %q, want the last three", got)
	}
}

func TestLogRingTail(t *testing.T) {
	t.Parallel()

	ring := newLogRing(100)
	for _, line := range []string{"a", "b", "c"} {
		ring.Write([]byte(line + "\n"))
	}
	if got := ring.Tail(2); got != "b\nc" {
		t.Errorf("Tail(2) = %q", got)
	}
	if got := ring.Tail(99); got != "a\nb\nc" {
		t.Errorf("Tail(99) = %q, want everything", got)
	}
}

func TestLogRingFollow(t *testing.T) {
	t.Parallel()

	ring := newLogRing(100)
	ring.Write([]byte("before\n"))

	lines, stop := ring.Follow()
	defer stop()

	// Follow delivers what comes next, not the backlog: the caller writes the
	// snapshot first, so replaying it here would duplicate every line.
	ring.Write([]byte("after\n"))
	select {
	case got := <-lines:
		if got != "after" {
			t.Errorf("first line = %q, want after", got)
		}
	default:
		t.Fatal("nothing was delivered to the follower")
	}

	stop()
	if _, open := <-lines; open {
		t.Error("channel still open after stop")
	}
	// Writing after stop must not panic on a closed channel.
	ring.Write([]byte("later\n"))
}

func TestLogRingStopIsIdempotent(t *testing.T) {
	t.Parallel()

	ring := newLogRing(10)
	_, stop := ring.Follow()
	stop()
	stop()
}

func TestLogRingDoesNotBlockOnASlowFollower(t *testing.T) {
	t.Parallel()

	// A follower that never reads must lose lines rather than stall the
	// function that is producing them.
	ring := newLogRing(10)
	_, stop := ring.Follow()
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			ring.Write([]byte("line\n"))
		}
	}()

	select {
	case <-done:
	case <-make(chan struct{}):
	}
}

func TestLogRingIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	ring := newLogRing(100)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ring.Write([]byte("x\n"))
				_ = ring.Snapshot()
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, stop := ring.Follow()
			stop()
		}()
	}
	wg.Wait()

	if got := len(ring.Snapshot()); got != 100 {
		t.Errorf("held %d lines, want the cap of 100", got)
	}
}
