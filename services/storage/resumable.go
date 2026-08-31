package storage

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"

	"github.com/monirz/cloudrig/core/tmp"
)

// A resumable upload arrives in chunks against a session the client opens
// first. Bytes are appended to a temp file as they come and only become an
// object when the last chunk lands, so an interrupted upload leaves nothing
// half-written and memory stays flat however large the object is.

// session is one resumable upload in progress.
type session struct {
	bucket  string
	project string
	write   Write

	mu     sync.Mutex
	file   *os.File
	offset int64
	total  int64 // -1 until the client declares the size
	done   bool
}

// sessions holds uploads in progress.
type sessions struct {
	dir string

	mu sync.Mutex
	m  map[string]*session
}

func newSessions() (*sessions, error) {
	dir, err := tmp.Dir("resumable")
	if err != nil {
		return nil, err
	}
	return &sessions{dir: dir, m: map[string]*session{}}, nil
}

func (s *sessions) open(project, bucket string, w Write) (string, *session, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)

	f, err := os.Create(s.dir + "/" + id)
	if err != nil {
		return "", nil, err
	}

	sess := &session{bucket: bucket, project: project, write: w, file: f, total: -1}
	s.mu.Lock()
	s.m[id] = sess
	s.mu.Unlock()
	return id, sess, nil
}

func (s *sessions) get(id string) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	return sess, ok
}

func (s *sessions) close(id string) {
	s.mu.Lock()
	sess, ok := s.m[id]
	delete(s.m, id)
	s.mu.Unlock()

	if ok {
		sess.mu.Lock()
		if sess.file != nil {
			name := sess.file.Name()
			sess.file.Close()
			_ = os.Remove(name)
			sess.file = nil
		}
		sess.mu.Unlock()
	}
}

func (s *sessions) cleanup() { _ = os.RemoveAll(s.dir) }

// startResumable opens a session and points the client at it.
func (a *API) startResumable(w http.ResponseWriter, r *http.Request, p transport.Params, write Write) error {
	var meta objectRequest
	if err := decodeJSON(r, &meta); err != nil {
		return err
	}
	if meta.Name != "" {
		write.Name = meta.Name
	}
	if write.Name == "" {
		return required("name")
	}
	if meta.ContentType != "" {
		write.ContentType = meta.ContentType
	}
	if write.ContentType == "" {
		write.ContentType = r.Header.Get("X-Upload-Content-Type")
	}
	write.Metadata = meta.Metadata

	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	if _, _, err := a.svc.bucket(r.Context(), project, p["bucket"]); err != nil {
		return err
	}

	id, _, err := a.sessions.open(project, p["bucket"], write)
	if err != nil {
		return gerr.Wrap(err, gerr.Internal, "opening a resumable session")
	}

	// The client follows Location verbatim, so it must be absolute and carry
	// everything the chunk requests need.
	location := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=resumable&upload_id=%s",
		baseURL(r), p["bucket"], id)
	w.Header().Set("Location", location)
	w.Header().Set("X-GUploader-UploadID", id)
	w.WriteHeader(http.StatusOK)
	return nil
}

// resumableChunk appends one chunk, or finalises the upload.
func (a *API) resumableChunk(w http.ResponseWriter, r *http.Request, uploadID string) error {
	sess, ok := a.sessions.get(uploadID)
	if !ok {
		return gerr.New(gerr.NotFound, "unknown or completed upload session").
			WithHTTPStatus(http.StatusNotFound).
			WithReason("notFound")
	}

	start, end, total, query, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		return err
	}

	sess.mu.Lock()
	if total >= 0 {
		sess.total = total
	}
	// A query carries no bytes: the client is asking how far it got.
	if query {
		offset := sess.offset
		complete := sess.total >= 0 && offset >= sess.total
		sess.mu.Unlock()
		if !complete {
			return a.resumeIncomplete(w, r, offset)
		}
		return a.finalizeResumable(w, r, uploadID, sess)
	}

	if start != sess.offset {
		offset := sess.offset
		sess.mu.Unlock()
		// The client is not where we are; tell it where to restart from.
		return a.resumeIncomplete(w, r, offset)
	}

	// Copied straight to the file: a chunk is never held whole.
	written, copyErr := io.Copy(sess.file, r.Body)
	sess.offset += written
	offset := sess.offset
	knownTotal := sess.total
	sess.mu.Unlock()

	if copyErr != nil {
		return gerr.Wrap(copyErr, gerr.Internal, "storing an uploaded chunk")
	}
	if end >= 0 && offset != end+1 {
		return gerr.Newf(gerr.InvalidArgument,
			"chunk declared bytes %d-%d but carried %d", start, end, written).
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}

	if knownTotal < 0 || offset < knownTotal {
		return a.resumeIncomplete(w, r, offset)
	}
	return a.finalizeResumable(w, r, uploadID, sess)
}

// resumeIncomplete tells the client the upload is not finished.
//
// The Go client sets X-GUploader-No-308 because 308 collides with the
// standardised Permanent Redirect, and then expects 200 with the real status in
// X-Http-Status-Code-Override. Answering a bare 308 to that client makes its
// transport follow a redirect instead.
func (a *API) resumeIncomplete(w http.ResponseWriter, r *http.Request, offset int64) error {
	if offset > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", offset-1))
	}
	w.Header().Set("X-Range-MD5", "")

	if strings.EqualFold(r.Header.Get("X-GUploader-No-308"), "yes") {
		w.Header().Set("X-Http-Status-Code-Override", "308")
		w.WriteHeader(http.StatusOK)
		return nil
	}
	w.WriteHeader(http.StatusPermanentRedirect)
	return nil
}

// finalizeResumable turns a completed session into an object.
func (a *API) finalizeResumable(w http.ResponseWriter, r *http.Request, uploadID string, sess *session) error {
	sess.mu.Lock()
	if sess.done {
		sess.mu.Unlock()
		return gerr.New(gerr.NotFound, "upload session already completed").
			WithHTTPStatus(http.StatusNotFound).
			WithReason("notFound")
	}
	sess.done = true
	file := sess.file
	write := sess.write
	project := sess.project
	sess.mu.Unlock()

	// Read back from the start: the bytes stream into the blob tree from the
	// file rather than being held anywhere.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return gerr.Wrap(err, gerr.Internal, "rewinding the uploaded content")
	}

	obj, err := a.svc.WriteObject(r.Context(), project, write, file)
	a.sessions.close(uploadID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toAPIObject(obj, baseURL(r)))
}

// parseContentRange reads the header a resumable chunk carries.
//
//	bytes 0-262143/1048576   a chunk, with the total known
//	bytes 0-262143/*         a chunk, total not yet declared
//	bytes *\/1048576          no bytes: how far did I get?
func parseContentRange(header string) (start, end, total int64, query bool, err error) {
	if header == "" {
		// No range at all: the whole object in one request.
		return 0, -1, -1, false, nil
	}
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes ")
	if !ok {
		return 0, 0, 0, false, badRange(header)
	}

	rangePart, totalPart, ok := strings.Cut(spec, "/")
	if !ok {
		return 0, 0, 0, false, badRange(header)
	}

	total = -1
	if totalPart != "*" {
		total, err = strconv.ParseInt(totalPart, 10, 64)
		if err != nil {
			return 0, 0, 0, false, badRange(header)
		}
	}

	if rangePart == "*" {
		return 0, -1, total, true, nil
	}

	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, false, badRange(header)
	}
	if start, err = strconv.ParseInt(startPart, 10, 64); err != nil {
		return 0, 0, 0, false, badRange(header)
	}
	if end, err = strconv.ParseInt(endPart, 10, 64); err != nil {
		return 0, 0, 0, false, badRange(header)
	}
	return start, end, total, false, nil
}

func badRange(header string) error {
	return gerr.Newf(gerr.InvalidArgument, "Invalid Content-Range header: %q", header).
		WithHTTPStatus(http.StatusBadRequest).
		WithReason("invalid").
		WithLocation("Content-Range", "header")
}
