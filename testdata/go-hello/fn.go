// Package gohello is a sample HTTP Cloud Function in its own module, so the
// generated shim has to resolve an import outside cloudrig's own tree.
package gohello

import (
	"encoding/json"
	"net/http"
)

// Handler is the only exported handler here, so cloudrig detects it and
// --entry-point is not needed.
func Handler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "cloudrig"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"hello": name})
}
