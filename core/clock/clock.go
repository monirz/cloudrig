// Package clock is the only place in cloudrig permitted to read wall-clock
// time or schedule against it. Everything else takes a Clock, so that tests can
// substitute FakeClock and drive time forward deterministically instead of
// sleeping. The lint package enforces that.
package clock

import "time"

// Timer is a scheduled callback that has not necessarily run yet.
type Timer interface {
	// Stop cancels the timer, reporting whether it did so before the callback
	// ran. A second Stop returns false.
	Stop() bool
}

// Clock is the emulator's sole source of time.
type Clock interface {
	Now() time.Time

	// AfterFunc schedules f to run once, d in the future. A non-positive d is
	// due immediately. The callback runs on an unspecified goroutine for a real
	// clock, and synchronously inside Advance for a fake one — so f must not
	// assume it holds any caller's lock.
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
