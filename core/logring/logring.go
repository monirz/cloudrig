// Package logring keeps the last lines a child process wrote.
//
// Both the function runner and the Cloud Run runner need it: a child's output
// is the only account of why it failed, and it has to be bounded or a chatty
// process grows the emulator without end.
package logring

import (
	"bytes"
	"strings"
	"sync"
)

// DefaultLines is how much of a child's output is kept. Enough to explain
// a crash or read a request trace, small enough that a chatty function cannot
// grow the emulator without bound.
const DefaultLines = 1000

// Ring keeps the last lines a function wrote and fans new ones out to
// followers. It is the function's stdout and stderr merged, in the order the
// child produced them.
type Ring struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial []byte
	subs    map[chan string]struct{}
}

func New(max int) *Ring {
	return &Ring{max: max, subs: map[chan string]struct{}{}}
}

// Write accepts arbitrary chunks and splits them into lines, holding an
// incomplete trailing line until its newline arrives.
func (l *Ring) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		l.appendLine(strings.TrimRight(string(l.partial[:i]), "\r"))
		l.partial = l.partial[i+1:]
	}
	return len(p), nil
}

// appendLine records a line and notifies followers. The caller holds the lock.
func (l *Ring) appendLine(line string) {
	l.lines = append(l.lines, line)
	if over := len(l.lines) - l.max; over > 0 {
		l.lines = l.lines[over:]
	}
	for ch := range l.subs {
		// Never block on a follower: a slow reader loses lines rather than
		// stalling the function's own output.
		select {
		case ch <- line:
		default:
		}
	}
}

// Snapshot returns the lines held right now.
func (l *Ring) Snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// Tail returns the last n lines as text, for attaching to an error.
func (l *Ring) Tail(n int) string {
	lines := l.Snapshot()
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Follow returns a channel of lines written from now on, and a function to stop
// following. The channel is buffered; a follower that stops reading drops lines
// rather than blocking the function.
func (l *Ring) Follow() (<-chan string, func()) {
	ch := make(chan string, 256)

	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.subs, ch)
			l.mu.Unlock()
			close(ch)
		})
	}
}
