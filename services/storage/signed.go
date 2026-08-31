package storage

import (
	"net/http"
	"strconv"

	"github.com/monirz/cloudrig/transport"
)

// Signed URLs are accepted, neither signature nor expiry verified.
//
// The signature proves the holder had a service account's private key. There is
// no such key here and no identity to check one against, so verifying would
// mean inventing an authority — the same reason every other request is
// unauthenticated.
//
// Expiry is not checked either, and that is not laziness. A URL is signed
// entirely on the client, stamped with the client's wall clock, and the
// emulator's notion of time is an injected Clock that a test may hold at any
// instant. Under MustStart's fake clock every real signed URL looks issued in
// the future. Reading real time here instead would put wall-clock time back
// into the emulator, which is the one thing the design forbids.
//
// So a signed URL works as an ordinary request that happens to carry extra
// query parameters, which is what makes code using them testable at all.

// uploadSigned writes an object from a signed PUT.
//
// A signed upload URL names the object in the path and carries the bytes as the
// whole body: there is no uploadType and no metadata part.
func (a *API) uploadSigned(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	pre, err := preconditionsOf(r.URL.Query())
	if err != nil {
		return err
	}

	obj, err := a.svc.WriteObject(r.Context(), project, Write{
		Bucket:        p["bucket"],
		Name:          p["object"],
		ContentType:   r.Header.Get("Content-Type"),
		Preconditions: pre,
	}, r.Body)
	if err != nil {
		return err
	}

	// The XML surface answers an upload with headers and no body, as GCS does.
	w.Header().Set("ETag", obj.ETag)
	w.Header().Set("X-Goog-Generation", strconv.FormatInt(obj.Generation, 10))
	w.Header().Set("X-Goog-Hash", "crc32c="+obj.CRC32C+",md5="+obj.MD5)
	w.WriteHeader(http.StatusOK)
	return nil
}

// deleteSigned removes an object from a signed DELETE.
func (a *API) deleteSigned(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	pre, err := preconditionsOf(r.URL.Query())
	if err != nil {
		return err
	}
	if err := a.svc.DeleteObject(r.Context(), project, p["bucket"], p["object"], pre); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
