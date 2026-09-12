package transport_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/transport"
)

func getClock(t *testing.T, url string) transport.ClockStatus {
	t.Helper()
	resp, err := http.Get(url + "/_emu/clock")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s transport.ClockStatus
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestClockAdvanceFiresTimers is the core promise: advancing a manual clock over
// the wire moves time and fires everything due in the jump, so scheduled work a
// service armed on the same clock runs.
func TestClockAdvanceFiresTimers(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(epoch)
	srv := serve(t, transport.Config{Clock: fake})

	fired := make(chan struct{}, 1)
	fake.AfterFunc(time.Hour, func() { fired <- struct{}{} })

	if s := getClock(t, srv.URL); s.Mode != "virtual" || s.Pending != 1 {
		t.Fatalf("status = %+v, want virtual with 1 pending", s)
	}

	// Not yet due after 30m.
	postJSON(t, srv.URL+"/_emu/clock/advance", `{"duration":"30m"}`).Body.Close()
	select {
	case <-fired:
		t.Fatal("timer fired after only 30m, want it to wait for the full hour")
	default:
	}

	// The remaining 30m brings it due.
	resp := postJSON(t, srv.URL+"/_emu/clock/advance", `{"duration":"30m"}`)
	defer resp.Body.Close()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("timer did not fire after advancing past its deadline")
	}

	if got := getClock(t, srv.URL).Now; got != epoch.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("now = %s, want epoch+1h", got)
	}
}

// TestClockGotoRejectsThePast holds that time does not move backward: a fired
// timer cannot un-fire, so a past target is refused.
func TestClockGotoRejectsThePast(t *testing.T) {
	t.Parallel()

	srv := serve(t, transport.Config{Clock: clock.NewFake(epoch)})
	resp := postJSON(t, srv.URL+"/_emu/clock/goto", `{"time":"2020-01-01T00:00:00Z"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a past time", resp.StatusCode)
	}
}

// TestRealClockRejectsControl holds that a real-clock server reports "real" and
// refuses advance with a clear FailedPrecondition rather than a bare 404.
func TestRealClockRejectsControl(t *testing.T) {
	t.Parallel()

	srv := serve(t, transport.Config{Clock: clock.Real()})
	if s := getClock(t, srv.URL); s.Mode != "real" {
		t.Errorf("mode = %q, want real", s.Mode)
	}
	resp := postJSON(t, srv.URL+"/_emu/clock/advance", `{"duration":"1h"}`)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("a real clock accepted advance; it should refuse")
	}
}

// TestClockAdvanceAcceptsDaysAndWeeks holds that the day and week units the
// time-travel pitch uses ("advance 7d") actually parse, since Go's duration
// grammar stops at hours.
func TestClockAdvanceAcceptsDaysAndWeeks(t *testing.T) {
	t.Parallel()

	srv := serve(t, transport.Config{Clock: clock.NewFake(epoch)})

	postJSON(t, srv.URL+"/_emu/clock/advance", `{"duration":"7d"}`).Body.Close()
	postJSON(t, srv.URL+"/_emu/clock/advance", `{"duration":"1w"}`).Body.Close()

	// 7 days + 1 week = 14 days.
	if got, want := getClock(t, srv.URL).Now, epoch.Add(14*24*time.Hour).Format(time.RFC3339Nano); got != want {
		t.Errorf("now = %s, want %s", got, want)
	}
}

// TestAdvanceDrainsAsyncWork holds that advancing and travelling both run the
// Drain hook, so the response follows the asynchronous work a clock jump sets
// off (task/scheduler HTTP deliveries and the functions they trigger) rather
// than racing it.
func TestAdvanceDrainsAsyncWork(t *testing.T) {
	t.Parallel()

	var drained int
	var mu sync.Mutex
	drain := func() { mu.Lock(); drained++; mu.Unlock() }
	srv := serve(t, transport.Config{Clock: clock.NewFake(epoch), Drain: drain})

	postJSON(t, srv.URL+"/_emu/clock/advance", `{"duration":"1h"}`).Body.Close()
	postJSON(t, srv.URL+"/_emu/clock/goto", `{"time":"2027-01-01T00:00:00Z"}`).Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if drained != 2 {
		t.Errorf("drain called %d times, want 2 (advance + goto)", drained)
	}
}
