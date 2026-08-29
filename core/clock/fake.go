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
		// Callbacks see their own deadline, so a chain sees a monotonic clock.
		c.now = t.when

		// The callback may call back into this clock, so drop the lock.
		c.mu.Unlock()
		t.f()
		c.mu.Lock()
	}

	c.now = target
	c.in = false
	c.mu.Unlock()
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
