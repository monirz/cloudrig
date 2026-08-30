package storage

import (
	"strconv"
	"time"
)

// The JSON API spells several numbers as strings — generation, metageneration
// and size all carry `json:",string"` in Google's generated client. Emitting
// them as JSON numbers makes a real client fail to decode, so the wire types
// below are separate from the stored ones rather than reusing them.

type apiBucket struct {
	Kind           string         `json:"kind"`
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Location       string         `json:"location"`
	StorageClass   string         `json:"storageClass"`
	Versioning     *apiVersioning `json:"versioning,omitempty"`
	Metageneration string         `json:"metageneration"`
	TimeCreated    string         `json:"timeCreated"`
	Updated        string         `json:"updated"`
	SelfLink       string         `json:"selfLink,omitempty"`
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

// bucketRequest is the body of a bucket create.
type bucketRequest struct {
	Name         string         `json:"name,omitempty"`
	Location     string         `json:"location,omitempty"`
	StorageClass string         `json:"storageClass,omitempty"`
	Versioning   *apiVersioning `json:"versioning,omitempty"`
}

func toAPIBucket(b Bucket, base string) apiBucket {
	return apiBucket{
		Kind:           "storage#bucket",
		ID:             b.Name,
		Name:           b.Name,
		Location:       b.Location,
		StorageClass:   b.StorageClass,
		Versioning:     &apiVersioning{Enabled: b.Versioning},
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
