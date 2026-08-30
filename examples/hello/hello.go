// Package hello is a sample HTTP Cloud Function.
package hello

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HelloHTTP greets the caller. This is the signature Cloud Functions expects
// for an HTTP trigger: a plain http.HandlerFunc.
func HelloHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"greeting": fmt.Sprintf("hello, %s", name),
		"method":   r.Method,
		"path":     r.URL.Path,
	})
}

// Echo returns the request body, to prove streaming works.
func Echo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	_, _ = io.Copy(w, r.Body)
}
