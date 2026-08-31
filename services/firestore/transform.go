package firestore

import (
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// applyTransforms resolves a write's field transforms against the document it
// is producing, returning what each transform evaluated to.
//
// A transform is computed by the server, not the client: ServerTimestamp and
// Increment exist precisely so the value does not depend on the caller's clock
// or on a read the caller did earlier.
func applyTransforms(doc *firestorepb.Document, transforms []*firestorepb.DocumentTransform_FieldTransform, now *timestamppb.Timestamp) ([]*firestorepb.Value, error) {
	if len(transforms) == 0 {
		return nil, nil
	}
	if doc.Fields == nil {
		doc.Fields = map[string]*firestorepb.Value{}
	}

	results := make([]*firestorepb.Value, 0, len(transforms))
	for _, tr := range transforms {
		path := tr.GetFieldPath()
		current, _ := lookup(doc.Fields, path)

		value, err := evaluate(tr, current, now)
		if err != nil {
			return nil, err
		}
		if value == nil {
			deletePath(doc.Fields, path)
		} else {
			setPath(doc.Fields, path, value)
		}
		results = append(results, value)
	}
	return results, nil
}

// evaluate computes one transform. A nil value means the field is removed.
func evaluate(tr *firestorepb.DocumentTransform_FieldTransform, current *firestorepb.Value, now *timestamppb.Timestamp) (*firestorepb.Value, error) {
	switch t := tr.GetTransformType().(type) {
	case *firestorepb.DocumentTransform_FieldTransform_SetToServerValue:
		if t.SetToServerValue != firestorepb.DocumentTransform_FieldTransform_REQUEST_TIME {
			return nil, status.Error(codes.InvalidArgument, "unknown server value")
		}
		return &firestorepb.Value{
			ValueType: &firestorepb.Value_TimestampValue{TimestampValue: now},
		}, nil

	case *firestorepb.DocumentTransform_FieldTransform_Increment:
		// An absent or non-numeric field starts from zero, as in real
		// Firestore: incrementing a field nobody has written yet works.
		return addNumbers(current, t.Increment), nil

	case *firestorepb.DocumentTransform_FieldTransform_Maximum:
		if current == nil || rank(current) != rankNumber || compare(t.Maximum, current) > 0 {
			return t.Maximum, nil
		}
		return current, nil

	case *firestorepb.DocumentTransform_FieldTransform_Minimum:
		if current == nil || rank(current) != rankNumber || compare(t.Minimum, current) < 0 {
			return t.Minimum, nil
		}
		return current, nil

	case *firestorepb.DocumentTransform_FieldTransform_AppendMissingElements:
		return appendMissing(current, t.AppendMissingElements.GetValues()), nil

	case *firestorepb.DocumentTransform_FieldTransform_RemoveAllFromArray:
		return removeAll(current, t.RemoveAllFromArray.GetValues()), nil
	}
	return nil, status.Error(codes.Unimplemented, "unsupported field transform")
}

// addNumbers keeps integer arithmetic integral: incrementing an integer by an
// integer must not quietly turn the field into a double.
func addNumbers(current, delta *firestorepb.Value) *firestorepb.Value {
	currentIsInt := current != nil && isInteger(current)
	if current == nil || rank(current) != rankNumber {
		currentIsInt = isInteger(delta)
	}

	if currentIsInt && isInteger(delta) {
		var base int64
		if current != nil && rank(current) == rankNumber {
			base = current.GetIntegerValue()
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{
			IntegerValue: base + delta.GetIntegerValue(),
		}}
	}

	var base float64
	if current != nil && rank(current) == rankNumber {
		base = numberOf(current)
	}
	return &firestorepb.Value{ValueType: &firestorepb.Value_DoubleValue{
		DoubleValue: base + numberOf(delta),
	}}
}

func isInteger(v *firestorepb.Value) bool {
	_, ok := v.GetValueType().(*firestorepb.Value_IntegerValue)
	return ok
}

// appendMissing adds only the elements not already there, which is what makes
// ArrayUnion idempotent.
func appendMissing(current *firestorepb.Value, add []*firestorepb.Value) *firestorepb.Value {
	out := existing(current)
	for _, v := range add {
		if !anyEqual(out, v) {
			out = append(out, proto.Clone(v).(*firestorepb.Value))
		}
	}
	return arrayValue(out)
}

func removeAll(current *firestorepb.Value, drop []*firestorepb.Value) *firestorepb.Value {
	out := make([]*firestorepb.Value, 0, len(existing(current)))
	for _, v := range existing(current) {
		if !anyEqual(drop, v) {
			out = append(out, v)
		}
	}
	return arrayValue(out)
}

// existing reads the array a transform is amending. A field that is absent or
// holds something else is treated as an empty array, which is how Firestore
// makes ArrayUnion work on a new field.
func existing(v *firestorepb.Value) []*firestorepb.Value {
	if v == nil || rank(v) != rankArray {
		return nil
	}
	return v.GetArrayValue().GetValues()
}

func arrayValue(vs []*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_ArrayValue{
		ArrayValue: &firestorepb.ArrayValue{Values: vs},
	}}
}
