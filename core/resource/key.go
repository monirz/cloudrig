// Package resource encodes GCP resources as store keys.
//
// The encoding decides what a prefix scan can express, so it is here rather
// than inside a service: listing, delimiter rollup and reset-by-project are all
// prefix operations over these keys.
package resource

import (
	"fmt"
	"strconv"
	"strings"
)

// sep separates an object name from its generation.
//
// NUL, not '#': '#' is legal in a GCS object name, so "a#b" at generation 5 and
// "a" at generation "b#5" would encode identically. NUL cannot appear in a
// name, and it sorts before every other byte, which keeps "a" ahead of "a/b".
const sep = "\x00"

// genDigits zero-pads the generation so keys sort in generation order. A
// microsecond timestamp needs 16 digits; 20 leaves room and never overflows an
// int64.
const genDigits = 20

// BucketIndex maps a bucket name to its project.
//
// GCS bucket names are globally unique and object URLs carry no project, so a
// request naming only a bucket has to resolve one. The index lives outside the
// per-project tree because that is exactly what it exists to look up.
func BucketIndex(bucket string) string {
	return "bx/" + bucket
}

// BlobRefs is the key counting how many generations reference a blob.
//
// Blobs are content-addressed, so two objects with identical bytes share one
// file. Dropping a generation may not remove it until nothing else points at
// it, and a count is what says so.
func BlobRefs(sha string) string {
	return "rc/" + sha
}

// Bucket is the key holding a bucket's metadata.
//
//	p/{project}/b/{bucket}
func Bucket(project, bucket string) string {
	return "p/" + project + "/b/" + bucket
}

// BucketPrefix is the prefix covering everything in a bucket, its own metadata
// key included.
func BucketPrefix(project, bucket string) string {
	return Bucket(project, bucket) + "/"
}

// ProjectPrefix covers every bucket in a project, so reset-by-project is one
// prefix delete rather than a scan.
func ProjectPrefix(project string) string {
	return "p/" + project + "/"
}

// BucketsPrefix covers a project's bucket metadata keys. A key under it holding
// a further slash belongs to a bucket's contents, not to the bucket itself.
func BucketsPrefix(project string) string {
	return "p/" + project + "/b/"
}

// IAM is the key holding a resource's IAM policy. An empty object means the
// bucket's own policy.
func IAM(project, bucket, object string) string {
	if object == "" {
		return Bucket(project, bucket) + "/iam"
	}
	return Bucket(project, bucket) + "/iam/o/" + object
}

// Live is the key of the pointer to an object's current generation.
//
//	p/{project}/b/{bucket}/live/{name}
func Live(project, bucket, object string) string {
	return Bucket(project, bucket) + "/live/" + object
}

// LivePrefix covers every live pointer in a bucket. Listing walks these, not
// the generation keys: one entry per visible object.
func LivePrefix(project, bucket string) string {
	return Bucket(project, bucket) + "/live/"
}

// Object is the key of one generation's metadata.
//
//	p/{project}/b/{bucket}/o/{name}\x00{generation:020d}
func Object(project, bucket, object string, generation int64) string {
	return ObjectPrefix(project, bucket, object) + pad(generation)
}

// ObjectPrefix covers every generation of one object, in generation order.
func ObjectPrefix(project, bucket, object string) string {
	return Bucket(project, bucket) + "/o/" + object + sep
}

// ParseObject splits an object key back into its parts.
func ParseObject(key string) (project, bucket, object string, generation int64, err error) {
	rest, ok := strings.CutPrefix(key, "p/")
	if !ok {
		return "", "", "", 0, fmt.Errorf("resource: %q is not an object key", key)
	}
	project, rest, ok = strings.Cut(rest, "/b/")
	if !ok {
		return "", "", "", 0, fmt.Errorf("resource: %q has no bucket", key)
	}
	bucket, rest, ok = strings.Cut(rest, "/o/")
	if !ok {
		return "", "", "", 0, fmt.Errorf("resource: %q is not an object key", key)
	}

	// The name may itself contain slashes and hashes, so the split is on the
	// last separator, not the first.
	i := strings.LastIndex(rest, sep)
	if i < 0 {
		return "", "", "", 0, fmt.Errorf("resource: %q has no generation", key)
	}
	object = rest[:i]

	generation, err = strconv.ParseInt(rest[i+len(sep):], 10, 64)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("resource: %q has a malformed generation: %w", key, err)
	}
	return project, bucket, object, generation, nil
}

// ObjectName recovers the object name from a live-pointer key.
func ObjectName(project, bucket, liveKey string) (string, error) {
	name, ok := strings.CutPrefix(liveKey, LivePrefix(project, bucket))
	if !ok {
		return "", fmt.Errorf("resource: %q is not a live pointer in %s/%s", liveKey, project, bucket)
	}
	return name, nil
}

func pad(generation int64) string {
	s := strconv.FormatInt(generation, 10)
	if len(s) >= genDigits {
		return s
	}
	return strings.Repeat("0", genDigits-len(s)) + s
}
