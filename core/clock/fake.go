package clock

import (
	"sort"
	"sync"
	"time"
)

// FakeClock is a Clock whose time only moves when a test moves it.
//
// It never sleeps and never polls: Advance walks the pending timers in
// timestamp order and runs each due callback synchronously on the calling
// goroutine. When Advance returns, every callback due within the window has
// already finished, so a test can assert on its effects immediately with no
// waiting and no flakiness.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
	seq uint64       // insertion order, to break ties between equal deadlines
	q   []*fakeTimer // sorted by (when, seq)
	in  bool         // true while Advance is running, to catch reentrancy
}

// NewFake returns a FakeClock reading now.
func NewFake(now time.Time) *FakeClock { return &FakeClock{now: now} }

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

	// Keep the queue sorted on insert rather than at Advance time, so the
	// ordering rule lives in exactly one place.
	i := sort.Search(len(c.q), func(i int) bool { return t.before(c.q[i]) })
	c.q = append(c.q, nil)
	copy(c.q[i+1:], c.q[i:])
	c.q[i] = t
	return t
}

// Advance moves time forward by d, running every callback that comes due,
// earliest first. A callback that schedules another timer within the same
// window has that timer run too, before Advance returns; the queue is drained
// past d, not merely swept once.
//
// Calling Advance from inside a callback panics rather than deadlocking.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	if c.in {
		c.mu.Unlock()
		panic("clock: FakeClock.Advance called reentrantly from a timer callback")
	}
	c.in = true
	target := c.now.Add(d)

	for len(c.q) > 0 && !c.q[0].when.After(target) {
		t := c.q[0]
		c.q = c.q[1:]
		t.fired = true
		// Callbacks observe the time they were scheduled for, not the target,
		// so a chain of timers sees a monotonically increasing clock.
		c.now = t.when

		// The callback may call Now, AfterFunc or Stop on this clock, so the
		// lock cannot be held across it.
		c.mu.Unlock()
		t.f()
		c.mu.Lock()
	}

	c.now = target
	c.in = false
	c.mu.Unlock()
}

// Pending reports how many timers are still scheduled. Test-facing; it lets a
// test assert that nothing was left dangling.
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
