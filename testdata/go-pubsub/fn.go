// Package pubsubfn is a sample function that runs on Pub/Sub messages.
package pubsubfn

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// backgroundEvent is the first-generation envelope: the message in data, and
// what happened in context. Data is base64 the way the wire carries it.
type backgroundEvent struct {
	Data struct {
		Data       string            `json:"data"`
		Attributes map[string]string `json:"attributes"`
		MessageID  string            `json:"messageId"`
	} `json:"data"`
	Context struct {
		EventType string `json:"eventType"`
		Resource  struct {
			Name    string `json:"name"`
			Service string `json:"service"`
		} `json:"resource"`
	} `json:"context"`
}

// Handler logs each message it is delivered.
func Handler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var e backgroundEvent
	if err := json.Unmarshal(body, &e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(e.Data.Data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Printf("FIRED %s %q from %s via %s attrs=%v\n",
		e.Context.EventType, decoded, e.Context.Resource.Name,
		e.Context.Resource.Service, e.Data.Attributes)
	w.WriteHeader(http.StatusNoContent)
}
