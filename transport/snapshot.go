package transport

import (
	"net/http"

	"github.com/monirz/cloudrig/core/gerr"
)

// handleSnapshot saves or restores emulator state: GET streams a snapshot
// archive, POST loads one back.
func (h *Handler) handleSnapshot(w http.ResponseWriter, r *http.Request, _ Params) error {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/x-tar")
		// A mid-stream error cannot unsend the header, so the client sees a
		// truncated archive rather than an error status. That is the honest
		// signal: a partial tar fails to read back.
		return h.snap.WriteSnapshot(w)
	case http.MethodPost:
		if err := h.snap.ReadSnapshot(r.Body); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	default:
		return gerr.New(gerr.InvalidArgument, "use GET to save or POST to restore a snapshot")
	}
}
