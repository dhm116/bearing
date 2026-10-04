package model

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestFactIDMatchesSpecVectors(t *testing.T) {
	const r = "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e"
	tests := []struct {
		name      string
		predicate string
		object    *modelv1alpha1.FactObject
		want      string
	}{
		{
			"relation", "owned_by", &modelv1alpha1.FactObject{SubjectId: "0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f"},
			"041d6c02c06fa8bf4c8d093d9a3dce979580c4d9dfb7ead9b89542451890be89",
		},
		{
			"attribute", "default_branch", &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue("main")},
			"563ed892892a24d6ef4e160702db05949694a854bc3e1ae27e2b2af5dde1bba8",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FactID(r, tt.predicate, tt.object)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFactIDRejectsBadObjects(t *testing.T) {
	for name, o := range map[string]*modelv1alpha1.FactObject{
		"empty":     {},
		"both":      {SubjectId: "x", Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue("y")},
		"no type":   {Value: structpb.NewStringValue("y")},
		"mismatch":  {Type: modelv1alpha1.ValueType_VALUE_TYPE_BOOL, Value: structpb.NewStringValue("y")},
		"nan float": {Type: modelv1alpha1.ValueType_VALUE_TYPE_FLOAT, Value: structpb.NewNumberValue(math.NaN())},
	} {
		if _, err := FactID("s", "p", o); err == nil {
			t.Errorf("%s: got no error, want one", name)
		}
	}
}

func TestCanonicalValue(t *testing.T) {
	tests := []struct {
		name string
		t    modelv1alpha1.ValueType
		in   any
		want string
	}{
		{"negative zero", modelv1alpha1.ValueType_VALUE_TYPE_FLOAT, math.Copysign(0, -1), `0`},
		{"time", modelv1alpha1.ValueType_VALUE_TYPE_TIME, "2026-09-28T03:30:00.1234567+02:00", `"2026-09-28T01:30:00.123456Z"`},
		{"json", modelv1alpha1.ValueType_VALUE_TYPE_JSON, map[string]any{"b": 1.0, "a": []any{true, nil}}, `{"a":[true,null],"b":1}`},
		{"bool", modelv1alpha1.ValueType_VALUE_TYPE_BOOL, true, `true`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := CanonicalValue(tt.t, mustValue(t, tt.in))
			if err != nil {
				t.Fatal(err)
			}
			got, err := EncodeJSON(v)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
	if _, err := CanonicalValue(modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED, structpb.NewBoolValue(true)); err == nil {
		t.Error("got no error for an unset type, want one")
	}
	if _, err := CanonicalValue(modelv1alpha1.ValueType_VALUE_TYPE_STRING, nil); err == nil {
		t.Error("got no error for a missing value, want one")
	}
}

func TestCanonicalJSON(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// RFC 8785 appendix and ECMAScript number formatting.
		{
			`[1e21, 1e20, 0.000001, 1e-7, 123.456, -0, 5e-324, 1.7976931348623157e308, 100, 0.1]`,
			`[1e+21,100000000000000000000,0.000001,1e-7,123.456,0,5e-324,1.7976931348623157e+308,100,0.1]`,
		},
		{
			`{"b":"€\n","a":"<>&\u0001","é":1,"😀":2,"ﬁ":3}`,
			"{\"a\":\"<>&\\u0001\",\"b\":\"€\\n\",\"é\":1,\"😀\":2,\"ﬁ\":3}",
		},
		{`{"x":{"z":false,"y":null},"w":"\"\\\t\r\b\f"}`, `{"w":"\"\\\t\r\b\f","x":{"y":null,"z":false}}`},
	}
	for _, tt := range tests {
		got, err := canonicalizeJSON([]byte(tt.in))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("canonicalize %s\n got %s\nwant %s", tt.in, got, tt.want)
		}
	}
	if _, err := canonicalJSON(math.Inf(1)); err == nil {
		t.Error("got no error for +Inf, want one")
	}
	if _, err := canonicalJSON(struct{}{}); err == nil {
		t.Error("got no error for an unsupported type, want one")
	}
}

func TestContentHashIgnoresFieldOrderAndWhitespace(t *testing.T) {
	a := &eventv1alpha1.Observation{}
	if err := DecodeJSON([]byte(`{"data":{"entity":{"kind":"Team","key":"t:team/a","attributes":{"b":1,"a":"x"}}}}`), a); err != nil {
		t.Fatal(err)
	}
	b := &eventv1alpha1.Observation{}
	if err := DecodeJSON([]byte(`{ "data": { "entity": { "attributes": { "a": "x", "b": 1.0 }, "key": "t:team/a", "kind": "Team" } } }`), b); err != nil {
		t.Fatal(err)
	}
	ha, err := ContentHash(a.GetData())
	if err != nil {
		t.Fatal(err)
	}
	hb, err := ContentHash(b.GetData())
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb || len(ha) != 64 {
		t.Fatalf("got %s and %s, want one 64-digit hash", ha, hb)
	}
	b.GetData().GetEntity().Deleted = true
	if hc, _ := ContentHash(b.GetData()); hc == ha {
		t.Fatal("got the same hash after a change, want a different one")
	}
}

func TestEncodeJSONKeepsExplicitNull(t *testing.T) {
	o, err := DecodeObservation(readFixture(t, "../../testdata/observations/valid/1-repository-codeowners.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeJSON(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"description":null`, `"topics":[]`, `"direction":"DIRECTION_OUT"`, `"time":"2026-09-28T01:30:00Z"`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("%s does not contain %s", b, want)
		}
	}
	if strings.Contains(string(b), "\n") || strings.Contains(string(b), `": `) || strings.Contains(string(b), `, "`) {
		t.Errorf("got insignificant whitespace in %s, want none", b)
	}
}
