package cloudfunctions

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
)

// UploadPath is where generateUploadUrl points a client.
//
// Real GCF hands out a signed GCS URL. cloudrig hands out one of its own: the
// emulator controls the address it returns, so the source can land here without
// a bucket, an object store, or a signature to verify.
const UploadPath = "/_emu/upload"

// uploadStore holds source archives between generateUploadUrl and create.
type uploadStore struct {
	dir string

	mu   sync.Mutex
	seen map[string]string // token -> archive path
}

func newUploadStore() (*uploadStore, error) {
	dir, err := os.MkdirTemp("", "cloudrig-upload-")
	if err != nil {
		return nil, err
	}
	return &uploadStore{dir: dir, seen: map[string]string{}}, nil
}

// issue returns a fresh upload token.
func (u *uploadStore) issue() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	u.mu.Lock()
	defer u.mu.Unlock()
	u.seen[token] = filepath.Join(u.dir, token+".zip")
	return token, nil
}

// path returns where a token's archive lives, if the token was issued here.
func (u *uploadStore) path(token string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.seen[token]
	return p, ok
}

func (u *uploadStore) cleanup() { _ = os.RemoveAll(u.dir) }

// generateUploadURL answers with an address on this emulator.
func (s *Service) generateUploadURL(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	token, err := s.uploads.issue()
	if err != nil {
		return gerr.Wrap(err, gerr.Internal, "issuing an upload token")
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"uploadUrl": baseURL(r) + UploadPath + "/" + token,
	})
}

// receiveUpload stores an archive PUT to an issued URL. It streams to disk
// rather than buffering: a source tree can be large, and nothing here needs the
// bytes in memory.
func (s *Service) receiveUpload(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	dest, ok := s.uploads.path(p["token"])
	if !ok {
		return gerr.New(gerr.NotFound, "unknown or expired upload URL").
			WithHTTPStatus(http.StatusNotFound).
			WithReason("notFound")
	}

	f, err := os.Create(dest)
	if err != nil {
		return gerr.Wrap(err, gerr.Internal, "opening the upload destination")
	}
	defer f.Close()

	if _, err := io.Copy(f, r.Body); err != nil {
		return gerr.Wrap(err, gerr.Internal, "storing the uploaded source")
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
