package transport

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/monirz/cloudrig/core/gerr"
)

// clockController is a Clock whose time a client can move. A manual-mode server
// runs on one; the real clock does not implement it, so the advance and set
// endpoints are simply not registered and the status endpoint reports "real".
type clockController interface {
	Now() time.Time
	Advance(d time.Duration)
	Pending() int
}

// ClockStatus is the /_emu/clock body.
type ClockStatus struct {
	// Mode is "manual" when the clock is client-controlled, else "real".
	Mode string `json:"mode"`
	Now  string `json:"now"`
	// Pending is the number of timers still scheduled — scheduled tasks, ack
	// deadlines, TTLs — waiting for the clock to reach them. Manual mode only.
	Pending int `json:"pending"`
}

func (h *Handler) clockStatus(w http.ResponseWriter, _ *http.Request, _ Params) error {
	body := ClockStatus{Mode: "real", Now: h.clk.Now().UTC().Format(time.RFC3339Nano)}
	if h.cc != nil {
		body.Mode = "manual"
		body.Pending = h.cc.Pending()
	}
	return writeJSON(w, body)
}

// errRealClock explains that time control needs a manual-mode server.
func errRealClock() error {
	return gerr.New(gerr.FailedPrecondition,
		"the clock is real; start the emulator with --clock manual to control time")
}

// clockAdvance moves a manual clock forward, firing every timer that comes due.
func (h *Handler) clockAdvance(w http.ResponseWriter, r *http.Request, _ Params) error {
	if h.cc == nil {
		return errRealClock()
	}
	var req struct {
		Duration string `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return gerr.New(gerr.InvalidArgument, "a JSON body with a duration is required")
	}
	d, err := time.ParseDuration(req.Duration)
	if err != nil {
		return gerr.Newf(gerr.InvalidArgument, "duration %q: %v", req.Duration, err)
	}
	if d < 0 {
		return gerr.New(gerr.InvalidArgument, "duration cannot be negative; the clock does not move backward")
	}
	h.cc.Advance(d)
	return h.clockStatus(w, r, nil)
}

// clockSet moves a manual clock to an absolute time. It cannot move backward:
// timers that already fired cannot un-fire, so a past target is refused rather
// than silently ignored.
func (h *Handler) clockSet(w http.ResponseWriter, r *http.Request, _ Params) error {
	if h.cc == nil {
		return errRealClock()
	}
	var req struct {
		Time string `json:"time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return gerr.New(gerr.InvalidArgument, "a JSON body with a time is required")
	}
	t, err := time.Parse(time.RFC3339, req.Time)
	if err != nil {
		return gerr.Newf(gerr.InvalidArgument, "time %q: want RFC3339, e.g. 2026-09-12T15:00:00Z", req.Time)
	}
	delta := t.Sub(h.cc.Now())
	if delta < 0 {
		return gerr.Newf(gerr.FailedPrecondition,
			"cannot set the clock to %s: it is before now (%s) and time does not move backward",
			t.UTC().Format(time.RFC3339), h.cc.Now().UTC().Format(time.RFC3339))
	}
	h.cc.Advance(delta)
	return h.clockStatus(w, r, nil)
}

func writeJSON(w http.ResponseWriter, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	return json.NewEncoder(w).Encode(body)
}
