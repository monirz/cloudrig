package firestore

import (
	"strings"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
)

// A field path is dotted: profile.name addresses name inside the map held by
// profile. Masks, transforms and filters all speak it, and all of them have to
// walk the nesting rather than treat the whole path as one key — a document
// with a literal "profile.name" field is not what the caller meant.

// splitPath breaks a field path into its segments.
func splitPath(path string) []string { return strings.Split(path, ".") }

// lookup reads a value at a path, reporting whether it is there.
func lookup(fields map[string]*firestorepb.Value, path string) (*firestorepb.Value, bool) {
	segments := splitPath(path)

	for i, seg := range segments {
		v, ok := fields[seg]
		if !ok {
			return nil, false
		}
		if i == len(segments)-1 {
			return v, true
		}
		m, ok := v.GetValueType().(*firestorepb.Value_MapValue)
		if !ok {
			return nil, false
		}
		fields = m.MapValue.GetFields()
	}
	return nil, false
}

// setPath writes a value at a path, creating the maps along the way. A
// non-map in the middle is replaced: the path the caller named wins.
func setPath(fields map[string]*firestorepb.Value, path string, value *firestorepb.Value) {
	segments := splitPath(path)

	for i, seg := range segments {
		if i == len(segments)-1 {
			fields[seg] = value
			return
		}

		next, ok := fields[seg].GetValueType().(*firestorepb.Value_MapValue)
		if !ok || next.MapValue == nil {
			created := &firestorepb.MapValue{Fields: map[string]*firestorepb.Value{}}
			fields[seg] = &firestorepb.Value{
				ValueType: &firestorepb.Value_MapValue{MapValue: created},
			}
			fields = created.Fields
			continue
		}
		if next.MapValue.Fields == nil {
			next.MapValue.Fields = map[string]*firestorepb.Value{}
		}
		fields = next.MapValue.Fields
	}
}

// deletePath removes the value at a path. Intermediate maps are left, as
// Firestore leaves them.
func deletePath(fields map[string]*firestorepb.Value, path string) {
	segments := splitPath(path)

	for i, seg := range segments {
		if i == len(segments)-1 {
			delete(fields, seg)
			return
		}
		m, ok := fields[seg].GetValueType().(*firestorepb.Value_MapValue)
		if !ok {
			return
		}
		fields = m.MapValue.GetFields()
	}
}
