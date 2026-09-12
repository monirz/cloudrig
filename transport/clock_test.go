package transport_test

import (
	"encoding/json"
	"net/http"
	"strings"
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

	if s := getClock(t, srv.URL); s.Mode != "manual" || s.Pending != 1 {
		t.Fatalf("status = %+v, want manual with 1 pending", s)
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

// TestClockSetRejectsThePast holds that time does not move backward: a fired
// timer cannot un-fire, so a past target is refused.
func TestClockSetRejectsThePast(t *testing.T) {
	t.Parallel()

	srv := serve(t, transport.Config{Clock: clock.NewFake(epoch)})
	resp := postJSON(t, srv.URL+"/_emu/clock/set", `{"time":"2020-01-01T00:00:00Z"}`)
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
