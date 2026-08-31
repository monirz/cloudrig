package firestore

import (
	"bytes"
	"math"
	"strings"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
)

// Firestore orders values across types, not only within one, so a query can
// sort a field that holds mixed types. This is that order.
const (
	rankNull = iota
	rankBool
	rankNumber
	rankTimestamp
	rankString
	rankBytes
	rankReference
	rankGeoPoint
	rankArray
	rankMap
)

func rank(v *firestorepb.Value) int {
	switch v.GetValueType().(type) {
	case *firestorepb.Value_BooleanValue:
		return rankBool
	case *firestorepb.Value_IntegerValue, *firestorepb.Value_DoubleValue:
		return rankNumber
	case *firestorepb.Value_TimestampValue:
		return rankTimestamp
	case *firestorepb.Value_StringValue:
		return rankString
	case *firestorepb.Value_BytesValue:
		return rankBytes
	case *firestorepb.Value_ReferenceValue:
		return rankReference
	case *firestorepb.Value_GeoPointValue:
		return rankGeoPoint
	case *firestorepb.Value_ArrayValue:
		return rankArray
	case *firestorepb.Value_MapValue:
		return rankMap
	}
	return rankNull
}

// numberOf reads an integer or a double as a float, so the two compare as one
// type the way Firestore treats them.
func numberOf(v *firestorepb.Value) float64 {
	if i, ok := v.GetValueType().(*firestorepb.Value_IntegerValue); ok {
		return float64(i.IntegerValue)
	}
	return v.GetDoubleValue()
}

// compare orders two values: negative, zero or positive. Values of different
// types are ordered by type.
func compare(a, b *firestorepb.Value) int {
	ra, rb := rank(a), rank(b)
	if ra != rb {
		return sign(ra - rb)
	}

	switch ra {
	case rankNull:
		return 0
	case rankBool:
		return compareBool(a.GetBooleanValue(), b.GetBooleanValue())
	case rankNumber:
		x, y := numberOf(a), numberOf(b)
		// NaN sorts before every other number and equals only itself, which is
		// the one place this cannot use <.
		switch {
		case math.IsNaN(x) && math.IsNaN(y):
			return 0
		case math.IsNaN(x):
			return -1
		case math.IsNaN(y):
			return 1
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case rankTimestamp:
		return a.GetTimestampValue().AsTime().Compare(b.GetTimestampValue().AsTime())
	case rankString:
		return strings.Compare(a.GetStringValue(), b.GetStringValue())
	case rankBytes:
		return bytes.Compare(a.GetBytesValue(), b.GetBytesValue())
	case rankReference:
		return strings.Compare(a.GetReferenceValue(), b.GetReferenceValue())
	case rankArray:
		return compareArrays(a.GetArrayValue().GetValues(), b.GetArrayValue().GetValues())
	}
	// Geopoints and maps compare equal here: ordering by them is rare enough
	// that getting it wrong quietly is worse than treating them as ties.
	return 0
}

// compareArrays orders element by element, shorter first when one is a prefix.
func compareArrays(a, b []*firestorepb.Value) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return sign(len(a) - len(b))
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// fieldOf resolves a dotted field path against a document, so a filter can
// name a field inside a map.
func fieldOf(doc *firestorepb.Document, path string) (*firestorepb.Value, bool) {
	if path == "__name__" {
		return &firestorepb.Value{
			ValueType: &firestorepb.Value_ReferenceValue{ReferenceValue: doc.GetName()},
		}, true
	}

	fields := doc.GetFields()
	segments := strings.Split(path, ".")
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
