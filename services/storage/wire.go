package storage

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The JSON API spells several numbers as strings — generation, metageneration
// and size all carry `json:",string"` in Google's generated client. Emitting
// them as JSON numbers makes a real client fail to decode, so the wire types
// below are separate from the stored ones rather than reusing them.

type apiBucket struct {
	Kind           string            `json:"kind"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Location       string            `json:"location"`
	StorageClass   string            `json:"storageClass"`
	Versioning     *apiVersioning    `json:"versioning,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Metageneration string            `json:"metageneration"`
	TimeCreated    string            `json:"timeCreated"`
	Updated        string            `json:"updated"`
	SelfLink       string            `json:"selfLink,omitempty"`
}

type apiObject struct {
	Kind           string            `json:"kind"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Bucket         string            `json:"bucket"`
	Generation     string            `json:"generation"`
	Metageneration string            `json:"metageneration"`
	ContentType    string            `json:"contentType,omitempty"`
	Size           string            `json:"size"`
	MD5Hash        string            `json:"md5Hash,omitempty"`
	CRC32C         string            `json:"crc32c,omitempty"`
	ETag           string            `json:"etag,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	TimeCreated    string            `json:"timeCreated"`
	Updated        string            `json:"updated"`
	MediaLink      string            `json:"mediaLink,omitempty"`
	SelfLink       string            `json:"selfLink,omitempty"`
}

// objectRequest is the metadata body a client sends: on the multipart upload's
// first part, and on a PATCH.
type objectRequest struct {
	Name        string            `json:"name,omitempty"`
	ContentType string            `json:"contentType,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// apiVersioning is the nested object GCS reports versioning under.
type apiVersioning struct {
	Enabled bool `json:"enabled"`
}

// bucketRequest is the body of a bucket create or patch. A label's value is a
// pointer because a patch removes a label by sending it as null.
type bucketRequest struct {
	Name         string         `json:"name,omitempty"`
	Location     string         `json:"location,omitempty"`
	StorageClass string         `json:"storageClass,omitempty"`
	Versioning   *apiVersioning `json:"versioning,omitempty"`
	Labels       labelsField    `json:"labels"`
}

// labelsField is a request's labels. JSON alone cannot tell "labels" absent
// from "labels": null, and a patch means different things by them: leave the
// labels alone, or remove them all.
type labelsField struct {
	Present bool               // the key appeared, even as null
	Values  map[string]*string // nil for null; a nil value removes that key
}

func (l *labelsField) UnmarshalJSON(raw []byte) error {
	l.Present = true
	return json.Unmarshal(raw, &l.Values)
}

func toAPIBucket(b Bucket, base string) apiBucket {
	return apiBucket{
		Kind:           "storage#bucket",
		ID:             b.Name,
		Name:           b.Name,
		Location:       b.Location,
		StorageClass:   b.StorageClass,
		Versioning:     &apiVersioning{Enabled: b.Versioning},
		Labels:         b.Labels,
		Metageneration: strconv.FormatInt(b.Metageneration, 10),
		TimeCreated:    rfc3339(b.Created),
		Updated:        rfc3339(b.Updated),
		SelfLink:       base + "/storage/v1/b/" + b.Name,
	}
}

func toAPIObject(o Object, base string) apiObject {
	generation := strconv.FormatInt(o.Generation, 10)
	self := base + "/storage/v1/b/" + o.Bucket + "/o/" + escapeObject(o.Name)

	return apiObject{
		Kind:           "storage#object",
		ID:             o.Bucket + "/" + o.Name + "/" + generation,
		Name:           o.Name,
		Bucket:         o.Bucket,
		Generation:     generation,
		Metageneration: strconv.FormatInt(o.Metageneration, 10),
		ContentType:    o.ContentType,
		Size:           strconv.FormatInt(o.Size, 10),
		MD5Hash:        o.MD5,
		CRC32C:         o.CRC32C,
		ETag:           o.ETag,
		Metadata:       o.Metadata,
		TimeCreated:    rfc3339(o.Created),
		Updated:        rfc3339(o.Updated),
		MediaLink:      self + "?alt=media",
		SelfLink:       self,
	}
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// composeRequest is the body of a compose.
type composeRequest struct {
	SourceObjects []composeSource `json:"sourceObjects"`
	Destination   objectRequest   `json:"destination"`
}

type composeSource struct {
	Name                string               `json:"name"`
	Generation          wireInt64            `json:"generation,omitempty"`
	ObjectPreconditions *composePrecondition `json:"objectPreconditions,omitempty"`
}

// composePrecondition is the one condition GCS lets a compose part carry.
type composePrecondition struct {
	IfGenerationMatch *wireInt64 `json:"ifGenerationMatch,omitempty"`
}

// wireInt64 is an int64 the JSON API may spell as a string, as Google's client
// does for generation, or as a number, as a hand-written request may. A null,
// which the Python client sends for a source made with bucket.blob(name),
// leaves it unset.
type wireInt64 int64

func (n *wireInt64) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	if err != nil {
		return err
	}
	*n = wireInt64(v)
	return nil
}

type apiManagedFolder struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	Bucket         string `json:"bucket"`
	Metageneration string `json:"metageneration"`
	CreateTime     string `json:"createTime"`
	UpdateTime     string `json:"updateTime"`
	SelfLink       string `json:"selfLink,omitempty"`
}

func toAPIManagedFolder(f ManagedFolder, base string) apiManagedFolder {
	return apiManagedFolder{
		Kind:           "storage#managedFolder",
		ID:             f.Bucket + "/" + f.Name,
		Name:           f.Name,
		Bucket:         f.Bucket,
		Metageneration: strconv.FormatInt(f.Metageneration, 10),
		CreateTime:     rfc3339(f.Created),
		UpdateTime:     rfc3339(f.Updated),
		SelfLink:       base + "/storage/v1/b/" + f.Bucket + "/managedFolders/" + url.PathEscape(f.Name),
	}
}
