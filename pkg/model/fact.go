package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// FactID returns a fact's ID: the lowercase hex SHA-256 of the JCS form of
// {subject_id, predicate, object}, with the value type in short form and the
// value in canonical form. Pass canonical (survivor) subject IDs.
func FactID(subjectID, predicate string, object *modelv1alpha1.FactObject) (string, error) {
	obj, err := factObjectJSON(object)
	if err != nil {
		return "", err
	}
	b, err := canonicalJSON(map[string]any{"subject_id": subjectID, "predicate": predicate, "object": obj})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func factObjectJSON(o *modelv1alpha1.FactObject) (any, error) {
	hasValue := o.GetType() != modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED || o.GetValue() != nil
	switch {
	case o.GetSubjectId() != "" && !hasValue:
		return map[string]any{"subject_id": o.GetSubjectId()}, nil
	case o.GetSubjectId() == "" && hasValue:
		v, err := CanonicalValue(o.GetType(), o.GetValue())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": ShortName(o.GetType()), "value": v.AsInterface()}, nil
	default:
		return nil, errors.New("fact object: want subject_id, or type and value")
	}
}

// CanonicalValue returns v in the canonical form for its type, or an error
// if v doesn't have that type or can't be made canonical (NaN, infinities,
// unparsable times).
func CanonicalValue(t modelv1alpha1.ValueType, v *structpb.Value) (*structpb.Value, error) {
	if v == nil {
		return nil, errors.New("value is missing")
	}
	switch t {
	case modelv1alpha1.ValueType_VALUE_TYPE_STRING:
		if _, ok := v.GetKind().(*structpb.Value_StringValue); !ok {
			return nil, errTypeMismatch
		}
		return v, nil
	case modelv1alpha1.ValueType_VALUE_TYPE_BOOL:
		if _, ok := v.GetKind().(*structpb.Value_BoolValue); !ok {
			return nil, errTypeMismatch
		}
		return v, nil
	case modelv1alpha1.ValueType_VALUE_TYPE_FLOAT:
		n, ok := v.GetKind().(*structpb.Value_NumberValue)
		if !ok {
			return nil, errTypeMismatch
		}
		f := n.NumberValue
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errNonFinite
		}
		if f == 0 {
			f = 0 // -0 becomes 0
		}
		return structpb.NewNumberValue(f), nil
	case modelv1alpha1.ValueType_VALUE_TYPE_TIME:
		s, ok := v.GetKind().(*structpb.Value_StringValue)
		if !ok {
			return nil, errTypeMismatch
		}
		at, err := time.Parse(time.RFC3339Nano, s.StringValue)
		if err != nil {
			return nil, fmt.Errorf("time value: %w", err)
		}
		return TimeValue(at), nil
	case modelv1alpha1.ValueType_VALUE_TYPE_JSON:
		raw, err := v.MarshalJSON()
		if err != nil {
			return nil, err
		}
		c, err := canonicalizeJSON(raw)
		if err != nil {
			return nil, err
		}
		out := &structpb.Value{}
		if err := out.UnmarshalJSON(c); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, errors.New("value type is not set")
	}
}

var errTypeMismatch = errors.New("value does not have its declared type")

// JCS returns the RFC 8785 form of v, for ordering and comparing values by
// their canonical bytes. It fails for NaN, infinities and text that is not
// UTF-8.
func JCS(v *structpb.Value) ([]byte, error) {
	return canonicalJSON(plain(v))
}

// plain is v.AsInterface without its habit of turning NaN and infinities
// into the strings "NaN" and "Infinity", which would collide with those
// strings.
func plain(v *structpb.Value) any {
	switch k := v.GetKind().(type) {
	case *structpb.Value_NumberValue:
		return k.NumberValue
	case *structpb.Value_ListValue:
		out := make([]any, len(k.ListValue.GetValues()))
		for i, e := range k.ListValue.GetValues() {
			out[i] = plain(e)
		}
		return out
	case *structpb.Value_StructValue:
		out := make(map[string]any, len(k.StructValue.GetFields()))
		for name, e := range k.StructValue.GetFields() {
			out[name] = plain(e)
		}
		return out
	}
	return v.AsInterface()
}

// ContentHash returns the content_hash part of an observation's ordering
// key: the lowercase hex SHA-256 of the JCS form of the ProtoJSON of its
// data (proto field names, enum value names, unset fields omitted).
func ContentHash(data *modelv1alpha1.ObservationData) (string, error) {
	b, err := marshalJSON.Marshal(data)
	if err != nil {
		return "", err
	}
	c, err := canonicalizeJSON(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}
