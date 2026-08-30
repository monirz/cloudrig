package cloudfunctions

import (
	"strconv"
	"sync"
	"time"
)

// operation is google.longrunning.Operation, which every mutating v1 method
// returns and gcloud then polls.
type operation struct {
	Name     string         `json:"name"`
	Done     bool           `json:"done"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

// operationStore keeps completed operations so a client that polls after the
// fact still finds one. Deploys here are synchronous, so operations are born
// done; the store exists to satisfy the poll, not to track progress.
type operationStore struct {
	mu   sync.Mutex
	seq  uint64
	byID map[string]operation
}

func newOperationStore() *operationStore {
	return &operationStore{byID: map[string]operation{}}
}

func (s *operationStore) complete(now time.Time, response map[string]any) operation {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	id := "op-" + strconv.FormatInt(now.UnixNano(), 36) + "-" + strconv.FormatUint(s.seq, 36)
	op := operation{Name: "operations/" + id, Done: true, Response: response}
	s.byID[id] = op
	return op
}

func (s *operationStore) get(id string) (operation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.byID[id]
	return op, ok
}
