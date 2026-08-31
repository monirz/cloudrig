package storage

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
)

// Prefixes are the path prefixes the JSON API claims.
var Prefixes = []string{"/storage/v1/", "/upload/storage/v1/", "/download/storage/v1/"}

// API is the Cloud Storage JSON API over a Service.
type API struct {
	svc      *Service
	router   *transport.Router
	sessions *sessions
}

// NewAPI wires the handlers.
func NewAPI(svc *Service) *API {
	a := &API{svc: svc, router: transport.NewRouter()}

	// A failure here leaves resumable uploads unavailable rather than the
	// whole service; every other upload type still works.
	if s, err := newSessions(); err == nil {
		a.sessions = s
	}

	a.router.Handle(http.MethodPost, "/storage/v1/b", a.createBucket)
	a.router.Handle(http.MethodGet, "/storage/v1/b", a.listBuckets)
	a.router.Handle(http.MethodGet, "/storage/v1/b/{bucket}", a.getBucket)
	a.router.Handle(http.MethodDelete, "/storage/v1/b/{bucket}", a.deleteBucket)

	a.router.Handle(http.MethodGet, "/storage/v1/b/{bucket}/o", a.listObjects)
	a.router.Handle(http.MethodGet, "/storage/v1/b/{bucket}/o/{object}", a.getObject)
	a.router.Handle(http.MethodGet, "/download/storage/v1/b/{bucket}/o/{object}", a.getObject)
	a.router.Handle(http.MethodPatch, "/storage/v1/b/{bucket}/o/{object}", a.patchObject)
	a.router.Handle(http.MethodPut, "/storage/v1/b/{bucket}/o/{object}", a.patchObject)
	a.router.Handle(http.MethodDelete, "/storage/v1/b/{bucket}/o/{object}", a.deleteObject)

	a.router.Handle(http.MethodPost, "/upload/storage/v1/b/{bucket}/o", a.upload)
	// A chunk may arrive as PUT from clients that prefer it; the Go client
	// POSTs to the session URI.
	a.router.Handle(http.MethodPut, "/upload/storage/v1/b/{bucket}/o", a.upload)

	// The download path the Go client's reader uses. It is the one piece of the
	// XML API surface that cannot be skipped: NewReader goes here, not to the
	// JSON API, so without it every read fails.
	a.router.Handle(http.MethodGet, "/{bucket}/{object}", a.downloadMedia)
	a.router.Handle(http.MethodHead, "/{bucket}/{object}", a.downloadMedia)
	return a
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.router.ServeHTTP(w, r) }

// Close removes any in-progress upload staging.
func (a *API) Close() error {
	if a.sessions != nil {
		a.sessions.cleanup()
	}
	return nil
}

func (a *API) createBucket(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project := r.URL.Query().Get("project")
	if project == "" {
		return required("project")
	}

	var body bucketRequest
	if err := decodeJSON(r, &body); err != nil {
		return err
	}

	b, err := a.svc.CreateBucket(r.Context(), Bucket{
		Name:         body.Name,
		Project:      project,
		Location:     body.Location,
		StorageClass: body.StorageClass,
		Versioning:   body.Versioning != nil && body.Versioning.Enabled,
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toAPIBucket(b, baseURL(r)))
}

func (a *API) listBuckets(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project := r.URL.Query().Get("project")
	if project == "" {
		return required("project")
	}

	buckets, err := a.svc.ListBuckets(r.Context(), project)
	if err != nil {
		return err
	}
	items := make([]apiBucket, len(buckets))
	for i, b := range buckets {
		items[i] = toAPIBucket(b, baseURL(r))
	}
	return writeJSON(w, http.StatusOK, map[string]any{"kind": "storage#buckets", "items": items})
}

func (a *API) getBucket(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	b, err := a.svc.GetBucket(r.Context(), project, p["bucket"])
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toAPIBucket(b, baseURL(r)))
}

func (a *API) patchBucket(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	var body bucketRequest
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	pre, err := preconditionsOf(r.URL.Query())
	if err != nil {
		return err
	}

	b, err := a.svc.UpdateBucket(r.Context(), project, p["bucket"], BucketPatch{
		StorageClass:  body.StorageClass,
		Versioning:    versioningOf(body),
		Preconditions: pre,
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toAPIBucket(b, baseURL(r)))
}

// versioningOf distinguishes "not mentioned" from "set to false": a patch that
// omits versioning must leave it alone, not turn it off.
func versioningOf(body bucketRequest) *bool {
	if body.Versioning == nil {
		return nil
	}
	return &body.Versioning.Enabled
}

func (a *API) deleteBucket(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	if err := a.svc.DeleteBucket(r.Context(), project, p["bucket"]); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) listObjects(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	q := r.URL.Query()

	maxResults, err := optionalInt(q, "maxResults")
	if err != nil {
		return err
	}
	res, err := a.svc.ListObjects(r.Context(), project, ListRequest{
		Bucket:     p["bucket"],
		Prefix:     q.Get("prefix"),
		Delimiter:  q.Get("delimiter"),
		MaxResults: int(maxResults),
		PageToken:  q.Get("pageToken"),
	})
	if err != nil {
		return err
	}

	items := make([]apiObject, len(res.Objects))
	for i, o := range res.Objects {
		items[i] = toAPIObject(o, baseURL(r))
	}
	out := map[string]any{"kind": "storage#objects"}
	if len(items) > 0 {
		out["items"] = items
	}
	if len(res.Prefixes) > 0 {
		out["prefixes"] = res.Prefixes
	}
	if res.NextPageToken != "" {
		out["nextPageToken"] = res.NextPageToken
	}
	return writeJSON(w, http.StatusOK, out)
}

func (a *API) getObject(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	generation, err := optionalGeneration(r.URL.Query())
	if err != nil {
		return err
	}
	pre, err := preconditionsOf(r.URL.Query())
	if err != nil {
		return err
	}

	if r.URL.Query().Get("alt") != "media" {
		obj, err := a.svc.GetObjectIf(r.Context(), project, p["bucket"], p["object"], generation, pre)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, toAPIObject(obj, baseURL(r)))
	}

	obj, f, err := a.svc.OpenObject(r.Context(), project, p["bucket"], p["object"], generation, pre)
	if err != nil {
		return err
	}
	defer f.Close()

	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("X-Goog-Generation", strconv.FormatInt(obj.Generation, 10))
	w.Header().Set("X-Goog-Hash", "crc32c="+obj.CRC32C+",md5="+obj.MD5)
	// ServeContent streams the file and answers Range itself, so a download
	// costs no more memory than its buffer whatever the object's size.
	http.ServeContent(w, r, obj.Name, obj.Updated, f)
	return nil
}

// downloadMedia serves object content from /{bucket}/{object}.
func (a *API) downloadMedia(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	generation, err := optionalGeneration(r.URL.Query())
	if err != nil {
		return err
	}

	pre, err := preconditionsOf(r.URL.Query())
	if err != nil {
		return err
	}

	obj, f, err := a.svc.OpenObject(r.Context(), project, p["bucket"], p["object"], generation, pre)
	if err != nil {
		return err
	}
	defer f.Close()

	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("X-Goog-Generation", strconv.FormatInt(obj.Generation, 10))
	w.Header().Set("X-Goog-Metageneration", strconv.FormatInt(obj.Metageneration, 10))
	w.Header().Set("X-Goog-Hash", "crc32c="+obj.CRC32C+",md5="+obj.MD5)
	http.ServeContent(w, r, obj.Name, obj.Updated, f)
	return nil
}

func (a *API) patchObject(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	var body objectRequest
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	pre, err := preconditionsOf(r.URL.Query())
	if err != nil {
		return err
	}

	obj, err := a.svc.UpdateObject(r.Context(), project, p["bucket"], p["object"], Write{
		ContentType:   body.ContentType,
		Metadata:      body.Metadata,
		Preconditions: pre,
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toAPIObject(obj, baseURL(r)))
}

func (a *API) deleteObject(w http.ResponseWriter, r *http.Request, p transport.Params) error {
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

// upload handles uploadType=media and uploadType=multipart.
//
// The real Go client never sends media: it sends multipart under its chunk size
// and resumable above, so multipart is the path that matters. Resumable is
// declined loudly rather than half-served.
func (a *API) upload(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	project, err := a.svc.ProjectOf(r.Context(), p["bucket"])
	if err != nil {
		return err
	}
	q := r.URL.Query()
	pre, err := preconditionsOf(q)
	if err != nil {
		return err
	}

	write := Write{Bucket: p["bucket"], Name: q.Get("name"), Preconditions: pre}
	body := r.Body

	// A chunk names the session it belongs to; only the first request in a
	// resumable upload has no id.
	if id := q.Get("upload_id"); id != "" {
		if a.sessions == nil {
			return gerr.New(gerr.Internal, "no temporary directory for resumable uploads").
				WithHTTPStatus(http.StatusInternalServerError)
		}
		return a.resumableChunk(w, r, id)
	}

	switch uploadType := q.Get("uploadType"); uploadType {
	case "media", "":
		write.ContentType = r.Header.Get("Content-Type")

	case "multipart":
		meta, media, err := splitMultipart(r)
		if err != nil {
			return err
		}
		if meta.Name != "" {
			write.Name = meta.Name
		}
		write.ContentType = meta.ContentType
		write.Metadata = meta.Metadata
		body = media
		defer media.Close()

	case "resumable":
		if a.sessions == nil {
			return gerr.New(gerr.Internal, "no temporary directory for resumable uploads").
				WithHTTPStatus(http.StatusInternalServerError)
		}
		return a.startResumable(w, r, p, write)

	default:
		return gerr.Newf(gerr.InvalidArgument, "unknown uploadType %q", uploadType).
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}

	obj, err := a.svc.WriteObject(r.Context(), project, write, body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toAPIObject(obj, baseURL(r)))
}

// splitMultipart reads the metadata part and returns the media part unread, so
// the payload streams into the blob tree rather than being buffered here.
func splitMultipart(r *http.Request) (objectRequest, io.ReadCloser, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return objectRequest{}, nil, gerr.Wrap(err, gerr.InvalidArgument, "malformed multipart content type").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}
	boundary, ok := params["boundary"]
	if !ok {
		return objectRequest{}, nil, gerr.New(gerr.InvalidArgument, "multipart upload has no boundary").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}

	mr := multipart.NewReader(r.Body, boundary)

	metaPart, err := mr.NextPart()
	if err != nil {
		return objectRequest{}, nil, gerr.Wrap(err, gerr.InvalidArgument, "multipart upload has no metadata part").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}
	var meta objectRequest
	if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
		metaPart.Close()
		return objectRequest{}, nil, gerr.Wrap(err, gerr.InvalidArgument, "malformed object metadata").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}
	metaPart.Close()

	mediaPart, err := mr.NextPart()
	if err != nil {
		return objectRequest{}, nil, gerr.Wrap(err, gerr.InvalidArgument, "multipart upload has no media part").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}
	if meta.ContentType == "" {
		meta.ContentType = mediaPart.Header.Get("Content-Type")
	}
	return meta, mediaPart, nil
}

// preconditionsOf reads the precondition query parameters.
func preconditionsOf(q url.Values) (Preconditions, error) {
	var p Preconditions
	if raw := q.Get("ifGenerationMatch"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return p, invalidParam("ifGenerationMatch", raw)
		}
		p.IfGenerationMatch = &n
	}
	if raw := q.Get("ifGenerationNotMatch"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return p, invalidParam("ifGenerationNotMatch", raw)
		}
		p.IfGenerationNotMatch = &n
	}
	if raw := q.Get("ifMetagenerationMatch"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return p, invalidParam("ifMetagenerationMatch", raw)
		}
		p.IfMetagenerationMatch = &n
	}
	if raw := q.Get("ifMetagenerationNotMatch"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return p, invalidParam("ifMetagenerationNotMatch", raw)
		}
		p.IfMetagenerationNotMatch = &n
	}
	return p, nil
}

func optionalGeneration(q url.Values) (*int64, error) {
	raw := q.Get("generation")
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, invalidParam("generation", raw)
	}
	return &n, nil
}

func optionalInt(q url.Values, name string) (int64, error) {
	raw := q.Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, invalidParam(name, raw)
	}
	return n, nil
}

func decodeJSON(r *http.Request, into any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(into); err != nil && err != io.EOF {
		return gerr.Wrap(err, gerr.InvalidArgument, "malformed request body").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("parseError")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(body)
}

func required(name string) error {
	return gerr.Newf(gerr.InvalidArgument, "Required parameter: %s", name).
		WithHTTPStatus(http.StatusBadRequest).
		WithReason("required").
		WithLocation(name, "parameter")
}

func invalidParam(name, value string) error {
	return gerr.Newf(gerr.InvalidArgument, "Invalid value for %s: %s", name, value).
		WithHTTPStatus(http.StatusBadRequest).
		WithReason("invalid").
		WithLocation(name, "parameter")
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// escapeObject percent-encodes an object name for a URL path segment, so a name
// holding a slash stays one segment.
func escapeObject(name string) string {
	return strings.ReplaceAll(url.PathEscape(name), "/", "%2F")
}
