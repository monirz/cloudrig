package storage

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/core/resource"
	"github.com/monirz/cloudrig/store"
)

// Managed folders are prefixes of a bucket that carry their own IAM policy.
//
// Only what a pipeline uses to hand out per-prefix access is modelled:
// create, get, list, and a policy on each. The policy is stored and returned,
// never enforced, for the reason iam.go gives. A folder exists apart from the
// objects under it: creating one writes no object, and objects need no folder.

// ManagedFolder is a managed folder's metadata. Name always ends in a slash.
type ManagedFolder struct {
	Bucket         string    `json:"bucket"`
	Name           string    `json:"name"`
	Metageneration int64     `json:"metageneration"`
	Created        time.Time `json:"createTime"`
	Updated        time.Time `json:"updateTime"`

	// Incarnation is drawn at random on create. It goes into the policy etag,
	// so an etag read before a folder was deleted and created again never
	// matches the new folder's policy, whose store version starts over.
	Incarnation uint64 `json:"incarnation"`
}

// ManagedFolderName normalises a folder id as GCS does: a leading slash is
// dropped and a trailing one added, so "a/b" and "/a/b/" name the same folder.
func ManagedFolderName(id string) (string, error) {
	name := strings.TrimPrefix(id, "/")
	if name != "" && !strings.HasSuffix(name, "/") {
		name += "/"
	}
	switch {
	case name == "":
		return "", badFolderName(id, "must not be empty")
	case len(name) > 1024:
		return "", badFolderName(id, "must be at most 1024 bytes")
	case strings.Contains(name, "//"):
		return "", badFolderName(id, "must use a single / as delimiter")
	case strings.ContainsAny(name, "\x00\r\n"):
		return "", badFolderName(id, "must not contain NUL, CR or LF")
	}
	return name, nil
}

// CreateManagedFolder creates a folder in a bucket. A second create of the
// same name is AlreadyExists, as GCS answers.
func (s *Service) CreateManagedFolder(ctx context.Context, project, bucket, id string) (ManagedFolder, error) {
	name, err := ManagedFolderName(id)
	if err != nil {
		return ManagedFolder{}, err
	}
	if _, _, err := s.bucket(ctx, project, bucket); err != nil {
		return ManagedFolder{}, err
	}

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ManagedFolder{}, gerr.Wrap(err, gerr.Internal, "drawing the folder's incarnation")
	}
	now := s.clk.Now().UTC()
	f := ManagedFolder{
		Bucket: bucket, Name: name, Metageneration: 1, Created: now, Updated: now,
		Incarnation: binary.BigEndian.Uint64(nonce[:]),
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return ManagedFolder{}, gerr.Wrap(err, gerr.Internal, "encoding the managed folder")
	}
	if _, err := s.kv.Put(ctx, resource.ManagedFolder(project, bucket, name), raw, 0); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return ManagedFolder{}, gerr.Newf(gerr.AlreadyExists, "The managed folder you tried to create already exists: %s", name).
				WithHTTPStatus(http.StatusConflict).
				WithReason("conflict")
		}
		return ManagedFolder{}, gerr.Wrap(err, gerr.Internal, "storing the managed folder")
	}
	return f, nil
}

// GetManagedFolder returns a folder's metadata.
func (s *Service) GetManagedFolder(ctx context.Context, project, bucket, id string) (ManagedFolder, error) {
	name, err := ManagedFolderName(id)
	if err != nil {
		return ManagedFolder{}, err
	}
	if _, _, err := s.bucket(ctx, project, bucket); err != nil {
		return ManagedFolder{}, err
	}
	raw, _, err := s.kv.Get(ctx, resource.ManagedFolder(project, bucket, name))
	if errors.Is(err, store.ErrNotFound) {
		return ManagedFolder{}, gerr.Newf(gerr.NotFound, "The managed folder does not exist: %s", name).
			WithHTTPStatus(http.StatusNotFound).
			WithReason("notFound")
	}
	if err != nil {
		return ManagedFolder{}, gerr.Wrap(err, gerr.Internal, "reading the managed folder")
	}
	var f ManagedFolder
	if err := json.Unmarshal(raw, &f); err != nil {
		return ManagedFolder{}, gerr.Wrap(err, gerr.Internal, "decoding the managed folder")
	}
	return f, nil
}

// ListManagedFolders returns a bucket's folders whose names start with
// prefix, in name order, a page at a time. A limit of 0 means no limit.
func (s *Service) ListManagedFolders(ctx context.Context, project, bucket, prefix string, limit int, pageToken string) ([]ManagedFolder, string, error) {
	if _, _, err := s.bucket(ctx, project, bucket); err != nil {
		return nil, "", err
	}
	entries, next, err := s.kv.List(ctx, resource.ManagedFolderPrefix(project, bucket)+prefix, limit, pageToken)
	if err != nil {
		return nil, "", gerr.Wrap(err, gerr.Internal, "listing managed folders")
	}
	out := make([]ManagedFolder, 0, len(entries))
	for _, e := range entries {
		var f ManagedFolder
		if err := json.Unmarshal(e.Val, &f); err != nil {
			return nil, "", gerr.Wrap(err, gerr.Internal, "decoding a managed folder")
		}
		out = append(out, f)
	}
	return out, next, nil
}

// ManagedFolderPolicy returns a folder's stored policy, opaque to this layer,
// and its etag. With no policy set the policy is nil.
func (s *Service) ManagedFolderPolicy(ctx context.Context, project, bucket, id string) (policy, etag []byte, err error) {
	f, err := s.GetManagedFolder(ctx, project, bucket, id)
	if err != nil {
		return nil, nil, err
	}
	raw, version, err := s.kv.Get(ctx, resource.ManagedFolderIAM(project, bucket, f.Name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, policyEtag(f, 0), nil
	}
	if err != nil {
		return nil, nil, gerr.Wrap(err, gerr.Internal, "reading the managed folder's IAM policy")
	}
	return raw, policyEtag(f, version), nil
}

// SetManagedFolderPolicy stores a folder's policy and returns its new etag.
// With ifEtag set, the write happens only if the policy is still the one that
// etag was read from; a policy changed in between is Aborted, which is what
// IAM answers. A nil ifEtag overwrites whatever is there.
func (s *Service) SetManagedFolderPolicy(ctx context.Context, project, bucket, id string, policy, ifEtag []byte) ([]byte, error) {
	f, err := s.GetManagedFolder(ctx, project, bucket, id)
	if err != nil {
		return nil, err
	}
	key := resource.ManagedFolderIAM(project, bucket, f.Name)

	var ifVersion uint64
	if ifEtag != nil {
		incarnation, version, ok := parsePolicyEtag(ifEtag)
		if !ok {
			return nil, gerr.New(gerr.InvalidArgument, "The etag was not issued by this server.").
				WithHTTPStatus(http.StatusBadRequest)
		}
		if incarnation != f.Incarnation {
			return nil, concurrentPolicyChange()
		}
		ifVersion = version
	} else {
		_, current, err := s.kv.Get(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, gerr.Wrap(err, gerr.Internal, "reading the managed folder's IAM policy")
		}
		ifVersion = current
	}

	version, err := s.kv.Put(ctx, key, policy, ifVersion)
	if errors.Is(err, store.ErrVersionMismatch) {
		return nil, concurrentPolicyChange()
	}
	if err != nil {
		return nil, gerr.Wrap(err, gerr.Internal, "storing the managed folder's IAM policy")
	}
	return policyEtag(f, version), nil
}

// policyEtag is the folder's incarnation and the policy's store version. The
// version moves on every write; the incarnation keeps a deleted and re-created
// folder, whose versions start over, from matching an old etag.
func policyEtag(f ManagedFolder, version uint64) []byte {
	return binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, f.Incarnation), version)
}

func parsePolicyEtag(etag []byte) (incarnation, version uint64, ok bool) {
	if len(etag) != 16 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint64(etag[:8]), binary.BigEndian.Uint64(etag[8:]), true
}

func concurrentPolicyChange() error {
	return gerr.New(gerr.Aborted, "There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.").
		WithHTTPStatus(http.StatusConflict)
}

// DeleteFolderOptions are a managed folder delete's conditions.
type DeleteFolderOptions struct {
	// AllowNonEmpty deletes a folder with objects or folders under it.
	AllowNonEmpty bool
	// With either set, the delete happens only if the folder's
	// metageneration matches, or does not match, the value.
	IfMetagenerationMatch    *int64
	IfMetagenerationNotMatch *int64
}

// DeleteManagedFolder removes a folder and its policy. A folder with objects
// or other managed folders under it is refused unless opts.AllowNonEmpty, as
// GCS does; what is under it stays either way, since a managed folder never
// owned it.
func (s *Service) DeleteManagedFolder(ctx context.Context, project, bucket, id string, opts DeleteFolderOptions) error {
	f, err := s.GetManagedFolder(ctx, project, bucket, id)
	if err != nil {
		return err
	}
	if m := opts.IfMetagenerationMatch; m != nil && f.Metageneration != *m {
		return folderConditionNotMet(f, "is not")
	}
	if m := opts.IfMetagenerationNotMatch; m != nil && f.Metageneration == *m {
		return folderConditionNotMet(f, "is")
	}
	if !opts.AllowNonEmpty {
		live, _, err := s.kv.List(ctx, resource.LivePrefix(project, bucket)+f.Name, 1, "")
		if err != nil {
			return gerr.Wrap(err, gerr.Internal, "checking whether the managed folder is empty")
		}
		// A folder nested inside counts too. The folder's own key sorts first
		// under its prefix, so a second entry is a nested one.
		folders, _, err := s.kv.List(ctx, resource.ManagedFolder(project, bucket, f.Name), 2, "")
		if err != nil {
			return gerr.Wrap(err, gerr.Internal, "checking the managed folder for nested folders")
		}
		if len(live) > 0 || len(folders) > 1 {
			return gerr.Newf(gerr.FailedPrecondition, "The managed folder you tried to delete is not empty: %s. Pass allowNonEmpty to delete it anyway.", f.Name).
				WithHTTPStatus(http.StatusConflict).
				WithReason("conflict")
		}
	}
	// The policy goes first: a folder left without one reads as an empty
	// policy, while a policy left without its folder would come back on a
	// later create of the same name.
	if err := s.kv.Delete(ctx, resource.ManagedFolderIAM(project, bucket, f.Name), 0); err != nil && !errors.Is(err, store.ErrNotFound) {
		return gerr.Wrap(err, gerr.Internal, "deleting the managed folder's IAM policy")
	}
	if err := s.kv.Delete(ctx, resource.ManagedFolder(project, bucket, f.Name), 0); err != nil && !errors.Is(err, store.ErrNotFound) {
		return gerr.Wrap(err, gerr.Internal, "deleting the managed folder")
	}
	return nil
}

// hasManagedFolders reports whether a bucket holds any managed folder.
func (s *Service) hasManagedFolders(ctx context.Context, project, bucket string) (bool, error) {
	entries, _, err := s.kv.List(ctx, resource.ManagedFolderPrefix(project, bucket), 1, "")
	if err != nil {
		return false, gerr.Wrap(err, gerr.Internal, "checking the bucket for managed folders")
	}
	return len(entries) > 0, nil
}

func folderConditionNotMet(f ManagedFolder, rel string) error {
	return gerr.Newf(gerr.FailedPrecondition, "The managed folder's metageneration %d %s the one the request requires: %s", f.Metageneration, rel, f.Name).
		WithHTTPStatus(http.StatusPreconditionFailed).
		WithReason("conditionNotMet")
}

func badFolderName(id, why string) error {
	return gerr.Newf(gerr.InvalidArgument, "Invalid managed folder name %q: %s", id, why).
		WithHTTPStatus(http.StatusBadRequest).
		WithReason("invalid")
}
