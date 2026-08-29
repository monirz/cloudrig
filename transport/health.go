package transport

import (
	"encoding/json"
	"net/http"
)

// Health is the /_emu/health body.
type Health struct {
	Status  string     `json:"status"`
	Version string     `json:"version"`
	Uptime  string     `json:"uptime"`
	Runner  RunnerInfo `json:"runner"`
}

// health reports liveness, version and uptime. Uptime comes from the injected
// clock, which makes this an end-to-end check that the clock reaches bottom.
func (h *Handler) health(w http.ResponseWriter, r *http.Request, _ Params) error {
	body := Health{
		Status:  "ok",
		Version: h.version,
		Uptime:  h.clk.Now().Sub(h.started).String(),
		Runner:  h.runner,
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	return json.NewEncoder(w).Encode(body)
}
