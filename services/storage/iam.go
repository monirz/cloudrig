package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/core/resource"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/transport"
)

// IAM policies are stored and returned, never enforced.
//
// Every request here is unauthenticated, so there is no identity to evaluate a
// binding against — an emulator that refused a request on a policy would be
// inventing an authority it does not have. Storing them is still worth doing:
// Terraform's google_storage_bucket_iam_member does read-modify-write, and
// against an unrouted endpoint it retries a 404 as though it were eventual
// consistency, hanging rather than failing.

// Policy is an IAM policy as the JSON API spells it.
type Policy struct {
	Kind       string    `json:"kind"`
	ResourceID string    `json:"resourceId"`
	Version    int       `json:"version"`
	Etag       string    `json:"etag"`
	Bindings   []Binding `json:"bindings"`
}

// Binding grants a role to members.
type Binding struct {
	Role    string   `json:"role"`
	Members []string `json:"members"`
}

// GetIAMPolicy returns the stored policy, or an empty one.
func (s *Service) GetIAMPolicy(ctx context.Context, project, bucket, object string) (Policy, error) {
	if _, _, err := s.bucket(ctx, project, bucket); err != nil {
		return Policy{}, err
	}

	raw, version, err := s.kv.Get(ctx, resource.IAM(project, bucket, object))
	if errors.Is(err, store.ErrNotFound) {
		return emptyPolicy(project, bucket, object), nil
	}
	if err != nil {
		return Policy{}, gerr.Wrap(err, gerr.Internal, "reading the IAM policy")
	}

	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, gerr.Wrap(err, gerr.Internal, "decoding the IAM policy")
	}
	_ = version
	return p, nil
}

// SetIAMPolicy stores a policy and returns it as stored.
func (s *Service) SetIAMPolicy(ctx context.Context, project, bucket, object string, p Policy) (Policy, error) {
	if _, _, err := s.bucket(ctx, project, bucket); err != nil {
		return Policy{}, err
	}

	base := emptyPolicy(project, bucket, object)
	p.Kind, p.ResourceID = base.Kind, base.ResourceID
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Bindings == nil {
		p.Bindings = []Binding{}
	}
	// The etag moves on every write, so a client doing read-modify-write can
	// tell its copy is stale even though nothing here rejects it for being so.
	p.Etag = etagFor(p)

	encoded, err := json.Marshal(p)
	if err != nil {
		return Policy{}, gerr.Wrap(err, gerr.Internal, "encoding the IAM policy")
	}

	key := resource.IAM(project, bucket, object)
	_, version, err := s.kv.Get(ctx, key)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Policy{}, gerr.Wrap(err, gerr.Internal, "reading the IAM policy")
	}
	if _, err := s.kv.Put(ctx, key, encoded, version); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return Policy{}, preconditionFailed(bucket, object)
		}
		return Policy{}, gerr.Wrap(err, gerr.Internal, "storing the IAM policy")
	}
	return p, nil
}

func emptyPolicy(project, bucket, object string) Policy {
	id := "projects/_/buckets/" + bucket
	if object != "" {
		id += "/objects/" + object
	}
	return Policy{Kind: "storage#policy", ResourceID: id, Version: 1, Etag: "CAE=", Bindings: []Binding{}}
}

// etagFor is a stable function of the bindings. Clients treat it as opaque.
func etagFor(p Policy) string {
	n := 0
	for _, b := range p.Bindings {
		n += 1 + len(b.Members)
	}
	return "CA" + string(rune('A'+n%26)) + "="
}

// --- HTTP ---

func (a *API) getBucketIAM(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return a.readPolicy(w, r, p["bucket"], "")
}

func (a *API) setBucketIAM(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return a.writePolicy(w, r, p["bucket"], "")
}

func (a *API) getObjectIAM(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return a.readPolicy(w, r, p["bucket"], p["object"])
}

func (a *API) setObjectIAM(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return a.writePolicy(w, r, p["bucket"], p["object"])
}

func (a *API) readPolicy(w http.ResponseWriter, r *http.Request, bucket, object string) error {
	project, err := a.svc.ProjectOf(r.Context(), bucket)
	if err != nil {
		return err
	}
	policy, err := a.svc.GetIAMPolicy(r.Context(), project, bucket, object)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, policy)
}

func (a *API) writePolicy(w http.ResponseWriter, r *http.Request, bucket, object string) error {
	project, err := a.svc.ProjectOf(r.Context(), bucket)
	if err != nil {
		return err
	}
	var policy Policy
	if err := decodeJSON(r, &policy); err != nil {
		return err
	}
	stored, err := a.svc.SetIAMPolicy(r.Context(), project, bucket, object, policy)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, stored)
}

// testIAMPermissions grants whatever was asked. Nothing is enforced, so
// answering with an empty set would only make callers refuse to proceed.
func (a *API) testIAMPermissions(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	if _, err := a.svc.ProjectOf(r.Context(), p["bucket"]); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"kind":        "storage#testIamPermissionsResponse",
		"permissions": r.URL.Query()["permissions"],
	})
}
