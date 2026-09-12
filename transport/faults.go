package transport

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/monirz/cloudrig/core/faults"
	"github.com/monirz/cloudrig/core/gerr"
)

// FaultRule is the wire form of a fault rule. Latency is a Go duration string
// ("2s") rather than nanoseconds so the API reads the way the CLI writes it.
type FaultRule struct {
	Method  string  `json:"method,omitempty"`
	Path    string  `json:"path"`
	Status  int     `json:"status,omitempty"`
	Message string  `json:"message,omitempty"`
	Latency string  `json:"latency,omitempty"`
	Count   int     `json:"count,omitempty"`
	Rate    float64 `json:"rate,omitempty"`
}

// faultsList is the GET body.
type faultsList struct {
	Rules []FaultRule `json:"rules"`
}

// handleFaults arms, lists or clears fault rules: POST adds one, GET lists the
// armed rules, DELETE disarms all.
func (h *Handler) handleFaults(w http.ResponseWriter, r *http.Request, _ Params) error {
	switch r.Method {
	case http.MethodPost:
		return h.addFault(w, r)
	case http.MethodGet:
		rules := h.faults.Rules()
		out := faultsList{Rules: make([]FaultRule, 0, len(rules))}
		for _, ru := range rules {
			out.Rules = append(out.Rules, toWire(ru))
		}
		return writeJSON(w, out)
	case http.MethodDelete:
		h.faults.Clear()
		w.WriteHeader(http.StatusNoContent)
		return nil
	default:
		return gerr.New(gerr.InvalidArgument, "use POST to add, GET to list or DELETE to clear faults")
	}
}

func (h *Handler) addFault(w http.ResponseWriter, r *http.Request) error {
	var wire FaultRule
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		return gerr.New(gerr.InvalidArgument, "a JSON fault rule is required")
	}
	if wire.Path == "" {
		return gerr.New(gerr.InvalidArgument, "a fault needs a path prefix to match")
	}
	if wire.Rate < 0 || wire.Rate > 1 {
		return gerr.New(gerr.InvalidArgument, "rate must be between 0 and 1")
	}
	var latency time.Duration
	if wire.Latency != "" {
		d, err := time.ParseDuration(wire.Latency)
		if err != nil {
			return gerr.Newf(gerr.InvalidArgument, "latency %q: %v", wire.Latency, err)
		}
		latency = d
	}
	h.faults.Add(faults.Rule{
		Method:  wire.Method,
		Path:    wire.Path,
		Status:  wire.Status,
		Message: wire.Message,
		Latency: latency,
		Count:   wire.Count,
		Rate:    wire.Rate,
	})
	return writeJSON(w, wire)
}

func toWire(r faults.Rule) FaultRule {
	w := FaultRule{
		Method:  r.Method,
		Path:    r.Path,
		Status:  r.Status,
		Message: r.Message,
		Count:   r.Count,
		Rate:    r.Rate,
	}
	if r.Latency > 0 {
		w.Latency = r.Latency.String()
	}
	return w
}
