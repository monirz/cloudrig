// Package hello is a demo HTTP Cloud Function that logs every call.
package hello

import (
	"encoding/json"
	"log"
	"net/http"
)

func Hello(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	log.Printf("hello called: name=%s method=%s", name, r.Method)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"greeting": "hello, " + name})
}
