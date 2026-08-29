// Package clock is the only place permitted to read wall-clock time.
// Everything else takes a Clock so tests can drive time deterministically.
package clock

import "time"

// Timer is a scheduled callback that has not necessarily run yet.
type Timer interface {
	// Stop cancels the timer, reporting whether it beat the callback.
	Stop() bool
}

// Clock is the emulator's sole source of time.
type Clock interface {
	Now() time.Time

	// AfterFunc runs f once, d in the future; a non-positive d is due now.
	// f runs on an unspecified goroutine, or synchronously inside Advance.
	AfterFunc(d time.Duration, f func()) Timer
}

// Real returns a Clock backed by the time package.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{time.AfterFunc(d, f)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) Stop() bool { return r.t.Stop() }
