package firestore

import (
	"math"
	"testing"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

func str(s string) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_StringValue{StringValue: s}}
}
func num(i int64) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: i}}
}
func dbl(f float64) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_DoubleValue{DoubleValue: f}}
}
func boolean(b bool) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_BooleanValue{BooleanValue: b}}
}
func null() *firestorepb.Value { return &firestorepb.Value{} }
func arr(vs ...*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_ArrayValue{
		ArrayValue: &firestorepb.ArrayValue{Values: vs},
	}}
}

// TestCompareAcrossTypes pins Firestore's order over mixed types, which is
// what lets a query sort a field that does not hold one type.
func TestCompareAcrossTypes(t *testing.T) {
	t.Parallel()

	ascending := []*firestorepb.Value{
		null(),
		boolean(false),
		boolean(true),
		num(1),
		dbl(1.5),
		num(2),
		{ValueType: &firestorepb.Value_TimestampValue{
			TimestampValue: timestamppb.New(time.Unix(0, 0))}},
		str("a"),
		str("b"),
	}

	for i := 0; i < len(ascending)-1; i++ {
		if c := compare(ascending[i], ascending[i+1]); c >= 0 {
			t.Errorf("value %d did not sort before %d: compare = %d", i, i+1, c)
		}
		if c := compare(ascending[i+1], ascending[i]); c <= 0 {
			t.Errorf("the reverse comparison of %d and %d is not symmetric", i, i+1)
		}
	}
}

// TestCompareIntegersAndDoubles holds that the two number types compare as
// one, so 1 and 1.0 are the same value to a query.
func TestCompareIntegersAndDoubles(t *testing.T) {
	t.Parallel()

	if c := compare(num(1), dbl(1.0)); c != 0 {
		t.Errorf("compare(1, 1.0) = %d, want 0", c)
	}
	if c := compare(num(2), dbl(1.5)); c <= 0 {
		t.Errorf("compare(2, 1.5) = %d, want positive", c)
	}
}

// TestCompareNaN is the one case that cannot use <: NaN sorts before every
// number and equals only itself.
func TestCompareNaN(t *testing.T) {
	t.Parallel()

	nan := dbl(math.NaN())
	if c := compare(nan, nan); c != 0 {
		t.Errorf("compare(NaN, NaN) = %d, want 0", c)
	}
	if c := compare(nan, num(0)); c >= 0 {
		t.Errorf("compare(NaN, 0) = %d, want negative", c)
	}
	if !isNaN(nan) || isNaN(num(0)) {
		t.Error("isNaN disagrees with itself")
	}
}

// TestCompareArrays orders element by element, with a prefix sorting first.
func TestCompareArrays(t *testing.T) {
	t.Parallel()

	if c := compare(arr(num(1)), arr(num(1), num(2))); c >= 0 {
		t.Errorf("a prefix did not sort first: %d", c)
	}
	if c := compare(arr(num(1), num(3)), arr(num(1), num(2))); c <= 0 {
		t.Errorf("arrays did not order by their first difference: %d", c)
	}
}

// TestFieldOfWalksMaps is what lets a filter name a field inside a map.
func TestFieldOfWalksMaps(t *testing.T) {
	t.Parallel()

	doc := &firestorepb.Document{
		Name: base + "/c/d",
		Fields: map[string]*firestorepb.Value{
			"top": {ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{
				Fields: map[string]*firestorepb.Value{"inner": str("found")},
			}}},
		},
	}

	if v, ok := fieldOf(doc, "top.inner"); !ok || v.GetStringValue() != "found" {
		t.Errorf("fieldOf(top.inner) = %v, %v", v, ok)
	}
	if _, ok := fieldOf(doc, "top.missing"); ok {
		t.Error("fieldOf found a field that is not there")
	}
	if _, ok := fieldOf(doc, "top.inner.deeper"); ok {
		t.Error("fieldOf walked into a value that is not a map")
	}
	// __name__ is the document itself, which is how a query orders by name.
	if v, ok := fieldOf(doc, "__name__"); !ok || v.GetReferenceValue() != doc.Name {
		t.Errorf("fieldOf(__name__) = %v, %v", v, ok)
	}
}

// TestIncrementKeepsIntegersIntegral is the trap in Increment: adding an
// integer to an integer must not quietly produce a double, because the client
// decodes the field back into an int64.
func TestIncrementKeepsIntegersIntegral(t *testing.T) {
	t.Parallel()

	got := addNumbers(num(5), num(3))
	if !isInteger(got) || got.GetIntegerValue() != 8 {
		t.Errorf("5 + 3 = %#v, want the integer 8", got.GetValueType())
	}

	// An absent field starts from zero and keeps the delta's type.
	if got := addNumbers(nil, num(2)); !isInteger(got) || got.GetIntegerValue() != 2 {
		t.Errorf("nil + 2 = %#v, want the integer 2", got.GetValueType())
	}
	// A double anywhere makes the result a double.
	if got := addNumbers(num(1), dbl(0.5)); isInteger(got) || got.GetDoubleValue() != 1.5 {
		t.Errorf("1 + 0.5 = %#v, want the double 1.5", got.GetValueType())
	}
}

// TestArrayUnionIsIdempotent is why ArrayUnion exists rather than an append.
func TestArrayUnionIsIdempotent(t *testing.T) {
	t.Parallel()

	current := arr(str("a"), str("b"))
	once := appendMissing(current, []*firestorepb.Value{str("b"), str("c")})
	twice := appendMissing(once, []*firestorepb.Value{str("b"), str("c")})

	if len(once.GetArrayValue().GetValues()) != 3 {
		t.Fatalf("union = %v, want three elements", once.GetArrayValue().GetValues())
	}
	if len(twice.GetArrayValue().GetValues()) != 3 {
		t.Errorf("applying the same union twice changed the array")
	}
}

// TestArrayTransformsOnAMissingField holds that a transform works on a field
// nobody has written, which is how a set is built from nothing.
func TestArrayTransformsOnAMissingField(t *testing.T) {
	t.Parallel()

	got := appendMissing(nil, []*firestorepb.Value{str("first")})
	if len(got.GetArrayValue().GetValues()) != 1 {
		t.Errorf("union on an absent field = %v", got)
	}
	if got := removeAll(nil, []*firestorepb.Value{str("x")}); len(got.GetArrayValue().GetValues()) != 0 {
		t.Errorf("remove on an absent field = %v", got)
	}
}
