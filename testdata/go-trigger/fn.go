// Package trigger is a sample function that runs on Cloud Storage events.
package trigger

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// backgroundEvent is the first-generation envelope Cloud Functions delivers:
// the changed resource in data, and what happened to it in context.
type backgroundEvent struct {
	Data struct {
		Bucket string `json:"bucket"`
		Name   string `json:"name"`
		Size   string `json:"size"`
	} `json:"data"`
	Context struct {
		EventID   string `json:"eventId"`
		EventType string `json:"eventType"`
		Resource  struct {
			Name    string `json:"name"`
			Service string `json:"service"`
		} `json:"resource"`
	} `json:"context"`
}

// Handler logs each object change.
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
	fmt.Printf("FIRED %s gs://%s/%s (%s bytes) via %s\n",
		e.Context.EventType, e.Data.Bucket, e.Data.Name, e.Data.Size, e.Context.Resource.Service)
	w.WriteHeader(http.StatusNoContent)
}
