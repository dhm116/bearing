// Package model holds the helpers around Bearing's data model: the kind and
// predicate registry, keys, the observation envelope, ProtoJSON encoding,
// validation at the edges, and fact_id and content_hash.
//
// The message types themselves are generated from proto/bearing/model and
// proto/bearing/event (v1alpha1) into gen/go; protobuf is their only source
// of truth (docs/adr/0006-protobuf-contracts.md). docs/spec/data-model.md
// gives the rules this package checks.
package model

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// SpecVersion is the CloudEvents spec version every observation uses.
const SpecVersion = "1.0"

// ObservationType is the CloudEvents type of an observation. It stays v1
// while the spec is a pre-1.0 draft (docs/spec/README.md, "Versioning").
const ObservationType = "dev.bearing.observation.v1"

// ContentType is the datacontenttype of an observation.
const ContentType = "application/json"

// TimeLayout is the canonical form of a time value: UTC with exactly six
// fraction digits.
const TimeLayout = "2006-01-02T15:04:05.000000Z"

// NewObservation wraps data in a CloudEvents envelope from source, observed
// at at (truncated to microseconds). The id is the entity key and the time,
// so re-sending the same observation keeps its id.
func NewObservation(source string, at time.Time, data *modelv1alpha1.ObservationData) *eventv1alpha1.Observation {
	at = at.UTC().Truncate(time.Microsecond)
	return &eventv1alpha1.Observation{
		Specversion:     SpecVersion,
		Id:              data.GetEntity().GetKey() + "@" + at.Format(TimeLayout),
		Source:          source,
		Type:            ObservationType,
		Time:            timestamppb.New(at),
		Datacontenttype: ContentType,
		Data:            data,
	}
}

// TimeValue returns t as a canonical time attribute value.
func TimeValue(t time.Time) *structpb.Value {
	return structpb.NewStringValue(t.UTC().Truncate(time.Microsecond).Format(TimeLayout))
}

// TruncateTimes truncates every google.protobuf.Timestamp in m, at any
// depth, to microseconds, as the core does on ingest.
func TruncateTimes(m proto.Message) {
	truncate(m.ProtoReflect())
}

var timestampName = (&timestamppb.Timestamp{}).ProtoReflect().Descriptor().FullName()

func truncate(m protoreflect.Message) {
	if m.Descriptor().FullName() == timestampName {
		if ts, ok := m.Interface().(*timestamppb.Timestamp); ok {
			ts.Nanos -= ts.Nanos % 1000
		}
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Message() == nil:
		case fd.IsList():
			l := v.List()
			for i := range l.Len() {
				truncate(l.Get(i).Message())
			}
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					truncate(mv.Message())
					return true
				})
			}
		default:
			truncate(v.Message())
		}
		return true
	})
}

var (
	marshalJSON   = protojson.MarshalOptions{UseProtoNames: true}
	unmarshalJSON = protojson.UnmarshalOptions{}
)

// EncodeJSON returns m as ProtoJSON with proto field names and enum value
// names, on one line, with unset fields omitted. Its whitespace is stable,
// unlike protojson's own output.
func EncodeJSON(m proto.Message) ([]byte, error) {
	b, err := marshalJSON.Marshal(m)
	if err != nil {
		return nil, err
	}
	return compact(b)
}

// DecodeJSON parses ProtoJSON into m. Unknown fields are an error.
func DecodeJSON(b []byte, m proto.Message) error {
	if err := unmarshalJSON.Unmarshal(b, m); err != nil {
		return fmt.Errorf("decode %s: %w", m.ProtoReflect().Descriptor().Name(), err)
	}
	return nil
}

// DecodeObservation parses one ProtoJSON observation and validates it.
func DecodeObservation(b []byte) (*eventv1alpha1.Observation, error) {
	o := &eventv1alpha1.Observation{}
	if err := DecodeJSON(b, o); err != nil {
		return nil, err
	}
	return o, ValidateObservation(o)
}
