package contracts

import (
	"encoding/json"
	"fmt"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// candidateJSON is Candidate on the wire: the proto messages in ProtoJSON,
// so their oneofs, well-known types and enums keep their canonical form.
type candidateJSON struct {
	Entity    json.RawMessage   `json:"entity"`
	Relations []json.RawMessage `json:"relations,omitempty"`
	Span      string            `json:"span"`
}

// MarshalJSON encodes the entity and relations as ProtoJSON.
func (c Candidate) MarshalJSON() ([]byte, error) {
	out := candidateJSON{Span: c.Span, Entity: json.RawMessage("null")}
	if c.Entity != nil {
		b, err := model.EncodeJSON(c.Entity)
		if err != nil {
			return nil, fmt.Errorf("candidate entity: %w", err)
		}
		out.Entity = b
	}
	for i, r := range c.Relations {
		b, err := model.EncodeJSON(r)
		if err != nil {
			return nil, fmt.Errorf("candidate relation %d: %w", i, err)
		}
		out.Relations = append(out.Relations, b)
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes what MarshalJSON writes.
func (c *Candidate) UnmarshalJSON(b []byte) error {
	var in candidateJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*c = Candidate{Span: in.Span}
	if len(in.Entity) > 0 && string(in.Entity) != "null" {
		c.Entity = &modelv1alpha1.Entity{}
		if err := model.DecodeJSON(in.Entity, c.Entity); err != nil {
			return fmt.Errorf("candidate entity: %w", err)
		}
	}
	for i, raw := range in.Relations {
		r := &modelv1alpha1.Relation{}
		if err := model.DecodeJSON(raw, r); err != nil {
			return fmt.Errorf("candidate relation %d: %w", i, err)
		}
		c.Relations = append(c.Relations, r)
	}
	return nil
}
