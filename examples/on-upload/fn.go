// Package onupload is a Cloud Function that fires when an object is written to
// a bucket. The storage event arrives as the request body.
package onupload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type event struct {
	Data struct {
		Bucket string `json:"bucket"`
		Name   string `json:"name"`
		Size   string `json:"size"`
	} `json:"data"`
	Context struct {
		EventType string `json:"eventType"`
	} `json:"context"`
}

// Handler logs the object that triggered it.
func Handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var e event
	if err := json.Unmarshal(body, &e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Printf("%s: gs://%s/%s (%s bytes)\n",
		e.Context.EventType, e.Data.Bucket, e.Data.Name, e.Data.Size)
	w.WriteHeader(http.StatusNoContent)
}
