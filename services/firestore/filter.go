package firestore

import (
	"math"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// matches reports whether a document satisfies a filter. A nil filter matches
// everything, which is a query with no Where clause.
func matches(doc *firestorepb.Document, f *firestorepb.StructuredQuery_Filter) (bool, error) {
	if f == nil {
		return true, nil
	}

	switch t := f.GetFilterType().(type) {
	case *firestorepb.StructuredQuery_Filter_CompositeFilter:
		return matchComposite(doc, t.CompositeFilter)
	case *firestorepb.StructuredQuery_Filter_FieldFilter:
		return matchField(doc, t.FieldFilter)
	case *firestorepb.StructuredQuery_Filter_UnaryFilter:
		return matchUnary(doc, t.UnaryFilter)
	}
	return false, status.Error(codes.Unimplemented, "unsupported filter")
}

func matchComposite(doc *firestorepb.Document, f *firestorepb.StructuredQuery_CompositeFilter) (bool, error) {
	switch f.GetOp() {
	case firestorepb.StructuredQuery_CompositeFilter_AND:
		for _, sub := range f.GetFilters() {
			ok, err := matches(doc, sub)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil

	case firestorepb.StructuredQuery_CompositeFilter_OR:
		for _, sub := range f.GetFilters() {
			ok, err := matches(doc, sub)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	return false, status.Error(codes.Unimplemented, "unsupported composite operator")
}

func matchField(doc *firestorepb.Document, f *firestorepb.StructuredQuery_FieldFilter) (bool, error) {
	got, present := fieldOf(doc, f.GetField().GetFieldPath())
	want := f.GetValue()

	// A document without the field matches nothing: Firestore requires the
	// field to exist for a comparison to hold.
	if !present {
		return false, nil
	}

	switch f.GetOp() {
	case firestorepb.StructuredQuery_FieldFilter_EQUAL:
		return compare(got, want) == 0, nil
	case firestorepb.StructuredQuery_FieldFilter_NOT_EQUAL:
		return compare(got, want) != 0, nil
	case firestorepb.StructuredQuery_FieldFilter_LESS_THAN:
		return compare(got, want) < 0, nil
	case firestorepb.StructuredQuery_FieldFilter_LESS_THAN_OR_EQUAL:
		return compare(got, want) <= 0, nil
	case firestorepb.StructuredQuery_FieldFilter_GREATER_THAN:
		return compare(got, want) > 0, nil
	case firestorepb.StructuredQuery_FieldFilter_GREATER_THAN_OR_EQUAL:
		return compare(got, want) >= 0, nil

	case firestorepb.StructuredQuery_FieldFilter_IN:
		return anyEqual(want.GetArrayValue().GetValues(), got), nil
	case firestorepb.StructuredQuery_FieldFilter_NOT_IN:
		return !anyEqual(want.GetArrayValue().GetValues(), got), nil

	case firestorepb.StructuredQuery_FieldFilter_ARRAY_CONTAINS:
		return anyEqual(got.GetArrayValue().GetValues(), want), nil
	case firestorepb.StructuredQuery_FieldFilter_ARRAY_CONTAINS_ANY:
		for _, candidate := range want.GetArrayValue().GetValues() {
			if anyEqual(got.GetArrayValue().GetValues(), candidate) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, status.Errorf(codes.Unimplemented, "unsupported operator %s", f.GetOp())
}

func matchUnary(doc *firestorepb.Document, f *firestorepb.StructuredQuery_UnaryFilter) (bool, error) {
	got, present := fieldOf(doc, f.GetField().GetFieldPath())

	switch f.GetOp() {
	case firestorepb.StructuredQuery_UnaryFilter_IS_NULL:
		return present && rank(got) == rankNull, nil
	case firestorepb.StructuredQuery_UnaryFilter_IS_NOT_NULL:
		return present && rank(got) != rankNull, nil
	case firestorepb.StructuredQuery_UnaryFilter_IS_NAN:
		return present && isNaN(got), nil
	case firestorepb.StructuredQuery_UnaryFilter_IS_NOT_NAN:
		return present && !isNaN(got), nil
	}
	return false, status.Errorf(codes.Unimplemented, "unsupported unary operator %s", f.GetOp())
}

func isNaN(v *firestorepb.Value) bool {
	return rank(v) == rankNumber && math.IsNaN(numberOf(v))
}

func anyEqual(haystack []*firestorepb.Value, needle *firestorepb.Value) bool {
	for _, v := range haystack {
		if compare(v, needle) == 0 {
			return true
		}
	}
	return false
}
