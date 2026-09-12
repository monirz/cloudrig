package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/monirz/cloudrig/core/gerr"
)

// clockController is a Clock whose time a client can move. A virtual-clock
// server runs on one; the real clock does not implement it, so status reports
// "real" and advance/goto answer with a clear FailedPrecondition.
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
	// deadlines, TTLs — waiting for the clock to reach them. Virtual mode only.
	Pending int `json:"pending"`
}

func (h *Handler) clockStatus(w http.ResponseWriter, _ *http.Request, _ Params) error {
	body := ClockStatus{Mode: "real", Now: h.clk.Now().UTC().Format(time.RFC3339Nano)}
	if h.cc != nil {
		body.Mode = "virtual"
		body.Pending = h.cc.Pending()
	}
	return writeJSON(w, body)
}

// errRealClock explains that time travel needs a virtual-clock server.
func errRealClock() error {
	return gerr.New(gerr.FailedPrecondition,
		"the clock is real; start the emulator with --clock virtual to travel time")
}

// clockAdvance travels a virtual clock forward, firing every timer that comes due.
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
	d, err := parseTravel(req.Duration)
	if err != nil {
		return gerr.Newf(gerr.InvalidArgument, "duration %q: %v", req.Duration, err)
	}
	if d < 0 {
		return gerr.New(gerr.InvalidArgument, "duration cannot be negative; the clock only moves forward")
	}
	h.cc.Advance(d)
	return h.clockStatus(w, r, nil)
}

// parseTravel parses a Go duration extended with d (days) and w (weeks), so a
// time-travel command reads the way the feature is pitched: "7d", "2w", as well
// as the standard "90m", "36h". A single d/w unit is expanded; everything else,
// including mixed forms like "1h30m", falls through to time.ParseDuration.
func parseTravel(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		return scaledDuration(n, 24*time.Hour)
	}
	if n, ok := strings.CutSuffix(s, "w"); ok {
		return scaledDuration(n, 7*24*time.Hour)
	}
	return time.ParseDuration(s)
}

func scaledDuration(count string, unit time.Duration) (time.Duration, error) {
	n, err := strconv.ParseFloat(count, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number of units", count)
	}
	return time.Duration(n * float64(unit)), nil
}

// clockGoto travels a virtual clock to an absolute time. It only moves forward:
// timers that already fired cannot un-fire, so a past target is refused rather
// than silently ignored.
func (h *Handler) clockGoto(w http.ResponseWriter, r *http.Request, _ Params) error {
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
			"cannot travel to %s: it is before now (%s) and time only moves forward",
			t.UTC().Format(time.RFC3339), h.cc.Now().UTC().Format(time.RFC3339))
	}
	h.cc.Advance(delta)
	return h.clockStatus(w, r, nil)
}

func writeJSON(w http.ResponseWriter, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	return json.NewEncoder(w).Encode(body)
}
