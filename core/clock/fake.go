package clock

import (
	"sort"
	"sync"
	"time"
)

// FakeClock is a Clock whose time only moves when a test moves it.
// It never sleeps and never polls.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
	seq uint64       // insertion order, breaks ties between equal deadlines
	q   []*fakeTimer // sorted by (when, seq)
	in  bool         // true while Advance runs, to catch reentrancy
}

// NewFake returns a FakeClock reading now.
func NewFake(now time.Time) *FakeClock { return &FakeClock{now: now} }

// NewFakeNow returns a FakeClock seeded at the real wall-clock time. A
// manual-mode server uses it so timestamps start realistic, then hold still
// until a client advances the clock. Reading time.Now here is the point: this
// is the one hand-off from the wall clock into a clock the emulator controls.
func NewFakeNow() *FakeClock { return &FakeClock{now: time.Now().UTC()} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()

	t := &fakeTimer{c: c, when: c.now.Add(d), seq: c.seq, f: f}
	c.seq++

	// Sorted on insert, so the ordering rule lives in one place.
	i := sort.Search(len(c.q), func(i int) bool { return t.before(c.q[i]) })
	c.q = append(c.q, nil)
	copy(c.q[i+1:], c.q[i:])
	c.q[i] = t
	return t
}

// Advance moves time forward by d, running every callback that comes due in
// timestamp order and draining past d. Reentrant calls panic.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.runTo(c.now.Add(d))
	c.mu.Unlock()
}

// AdvanceTo moves time forward to target, running every callback that comes due,
// and reports whether the target was reachable. It returns false only when
// target is strictly before now, since time does not move backward; target equal
// to now is a no-op that still reports true. The comparison and the move happen
// under one lock, so a caller cannot be told the target is in the future and
// then have a concurrent jump move past it — concurrent absolute travel
// converges on the latest target instead of summing deltas or falsely
// succeeding on a stale check.
func (c *FakeClock) AdvanceTo(target time.Time) (reachable bool) {
	c.mu.Lock()
	if target.Before(c.now) {
		c.mu.Unlock()
		return false
	}
	if target.After(c.now) {
		c.runTo(target)
	}
	c.mu.Unlock()
	return true
}

// runTo fires every callback due at or before target, then sets now to target.
// The caller holds c.mu; it is dropped around each callback, which may call
// back into the clock. Reentrant entry from a callback panics.
func (c *FakeClock) runTo(target time.Time) {
	if c.in {
		// Unlock before panicking: the caller took the lock and its deferred
		// unlock is skipped by the panic, so leaving it held would deadlock
		// every later clock call.
		c.mu.Unlock()
		panic("clock: FakeClock advanced reentrantly from a timer callback")
	}
	c.in = true
	for len(c.q) > 0 && !c.q[0].when.After(target) {
		t := c.q[0]
		c.q = c.q[1:]
		t.fired = true
		// Callbacks see their own deadline, so a chain sees a monotonic clock.
		c.now = t.when

		c.mu.Unlock()
		t.f()
		c.mu.Lock()
	}
	c.now = target
	c.in = false
}

// Pending reports how many timers are still scheduled, so a test can assert
// nothing was left dangling.
func (c *FakeClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.q)
}

type fakeTimer struct {
	c     *FakeClock
	when  time.Time
	seq   uint64
	f     func()
	fired bool
}

func (t *fakeTimer) before(u *fakeTimer) bool {
	if t.when.Equal(u.when) {
		return t.seq < u.seq
	}
	return t.when.Before(u.when)
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()

	if t.fired {
		return false
	}
	for i, q := range t.c.q {
		if q == t {
			t.c.q = append(t.c.q[:i], t.c.q[i+1:]...)
			t.fired = true // a stopped timer cannot be stopped again
			return true
		}
	}
	return false
}
