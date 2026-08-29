package clock_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestAdvanceRunsDueCallbacksInOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		delays  []time.Duration // scheduled in this order, labelled "0", "1", ...
		advance time.Duration
		want    string // labels, in fire order
		pending int
	}{
		{
			name:    "nothing scheduled",
			advance: time.Hour,
			want:    "",
		},
		{
			name:    "fires in timestamp order, not scheduling order",
			delays:  []time.Duration{30 * time.Second, 10 * time.Second, 20 * time.Second},
			advance: time.Minute,
			want:    "1,2,0",
		},
		{
			name:    "equal deadlines break ties by scheduling order",
			delays:  []time.Duration{time.Second, time.Second, time.Second},
			advance: time.Minute,
			want:    "0,1,2",
		},
		{
			name:    "a timer due exactly at the target fires",
			delays:  []time.Duration{time.Minute},
			advance: time.Minute,
			want:    "0",
		},
		{
			name:    "a timer past the target does not fire",
			delays:  []time.Duration{time.Minute, time.Minute + time.Nanosecond},
			advance: time.Minute,
			want:    "0",
			pending: 1,
		},
		{
			name:    "non-positive delay is due at the next advance",
			delays:  []time.Duration{0, -time.Hour},
			advance: 0,
			want:    "1,0", // -1h sorts before 0
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := clock.NewFake(epoch)
			var fired []string
			for i, d := range tc.delays {
				label := string(rune('0' + i))
				c.AfterFunc(d, func() { fired = append(fired, label) })
			}

			c.Advance(tc.advance)

			if got := strings.Join(fired, ","); got != tc.want {
				t.Errorf("fire order = %q, want %q", got, tc.want)
			}
			if got := c.Pending(); got != tc.pending {
				t.Errorf("pending = %d, want %d", got, tc.pending)
			}
			if got, want := c.Now(), epoch.Add(tc.advance); !got.Equal(want) {
				t.Errorf("Now() = %v, want %v", got, want)
			}
		})
	}
}

func TestCallbackObservesItsOwnDeadline(t *testing.T) {
	t.Parallel()

	c := clock.NewFake(epoch)
	var seen []time.Time
	for _, d := range []time.Duration{10 * time.Second, 20 * time.Second} {
		c.AfterFunc(d, func() { seen = append(seen, c.Now()) })
	}

	c.Advance(time.Minute)

	want := []time.Time{epoch.Add(10 * time.Second), epoch.Add(20 * time.Second)}
	if len(seen) != len(want) {
		t.Fatalf("saw %d callbacks, want %d", len(seen), len(want))
	}
	for i := range want {
		if !seen[i].Equal(want[i]) {
			t.Errorf("callback %d saw %v, want %v", i, seen[i], want[i])
		}
	}
}

func TestAdvanceDrainsTimersScheduledByCallbacks(t *testing.T) {
	t.Parallel()

	// Advance drains past d, so a self-rescheduling callback terminates.
	c := clock.NewFake(epoch)
	n := 0
	var tick func()
	tick = func() {
		n++
		if n < 5 {
			c.AfterFunc(time.Second, tick)
		}
	}
	c.AfterFunc(time.Second, tick)

	c.Advance(10 * time.Second)

	if n != 5 {
		t.Errorf("ran %d times, want 5", n)
	}
	if c.Pending() != 0 {
		t.Errorf("pending = %d, want 0", c.Pending())
	}
}

func TestCallbackSchedulingBeyondTheWindowIsDeferred(t *testing.T) {
	t.Parallel()

	c := clock.NewFake(epoch)
	inner := false
	c.AfterFunc(time.Second, func() {
		c.AfterFunc(time.Hour, func() { inner = true })
	})

	c.Advance(time.Minute)
	if inner {
		t.Fatal("timer scheduled an hour out fired within a one-minute advance")
	}

	c.Advance(time.Hour)
	if !inner {
		t.Error("timer did not fire in a window that covers it")
	}
}

func TestStop(t *testing.T) {
	t.Parallel()

	t.Run("prevents the callback", func(t *testing.T) {
		t.Parallel()
		c := clock.NewFake(epoch)
		ran := false
		timer := c.AfterFunc(time.Second, func() { ran = true })

		if !timer.Stop() {
			t.Error("Stop() = false on a pending timer, want true")
		}
		c.Advance(time.Minute)
		if ran {
			t.Error("callback ran after Stop")
		}
	})

	t.Run("is false once more", func(t *testing.T) {
		t.Parallel()
		c := clock.NewFake(epoch)
		timer := c.AfterFunc(time.Second, func() {})
		timer.Stop()
		if timer.Stop() {
			t.Error("second Stop() = true, want false")
		}
	})

	t.Run("is false after firing", func(t *testing.T) {
		t.Parallel()
		c := clock.NewFake(epoch)
		timer := c.AfterFunc(time.Second, func() {})
		c.Advance(time.Minute)
		if timer.Stop() {
			t.Error("Stop() = true after the timer fired, want false")
		}
	})

	t.Run("works from inside another callback", func(t *testing.T) {
		t.Parallel()
		c := clock.NewFake(epoch)
		ran := false
		var victim clock.Timer
		c.AfterFunc(time.Second, func() { victim.Stop() })
		victim = c.AfterFunc(2*time.Second, func() { ran = true })

		c.Advance(time.Minute)
		if ran {
			t.Error("callback ran despite being stopped by an earlier timer")
		}
	})
}

func TestAdvanceIsNotReentrant(t *testing.T) {
	t.Parallel()

	c := clock.NewFake(epoch)
	c.AfterFunc(time.Second, func() {
		defer func() {
			if recover() == nil {
				t.Error("reentrant Advance did not panic")
			}
		}()
		c.Advance(time.Second)
	})
	c.Advance(time.Minute)
}

func TestConcurrentSchedulingIsSafe(t *testing.T) {
	t.Parallel()

	// -race guards the queue; the count guards the sorted insert.
	c := clock.NewFake(epoch)
	var mu sync.Mutex
	fired := 0

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.AfterFunc(time.Duration(i)*time.Millisecond, func() {
				mu.Lock()
				fired++
				mu.Unlock()
			})
		}(i)
	}
	wg.Wait()

	c.Advance(time.Second)
	if fired != 64 {
		t.Errorf("fired = %d, want 64", fired)
	}
}

func TestRealClockSatisfiesTheInterface(t *testing.T) {
	t.Parallel()

	c := clock.Real()
	if c.Now().IsZero() {
		t.Error("Real().Now() is the zero time")
	}

	done := make(chan struct{})
	timer := c.AfterFunc(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("real timer never fired")
	}
	if timer.Stop() {
		t.Error("Stop() = true after the timer fired, want false")
	}
}

// TestAcceptance3 is acceptance criterion 3: 1s/5s/10s, Advance(6s) runs
// exactly two, in order, before returning.
func TestAcceptance3(t *testing.T) {
	t.Parallel()

	c := clock.NewFake(epoch)
	var fired []string
	for _, tc := range []struct {
		label string
		d     time.Duration
	}{
		{"1s", time.Second},
		{"5s", 5 * time.Second},
		{"10s", 10 * time.Second},
	} {
		label := tc.label
		c.AfterFunc(tc.d, func() { fired = append(fired, label) })
	}

	c.Advance(6 * time.Second)

	// Unsynchronised on purpose: if Advance returned early, -race says so.
	if got := strings.Join(fired, ","); got != "1s,5s" {
		t.Errorf("fired = %q, want %q", got, "1s,5s")
	}
	if got := c.Pending(); got != 1 {
		t.Errorf("pending = %d, want 1 (the 10s timer)", got)
	}
}
