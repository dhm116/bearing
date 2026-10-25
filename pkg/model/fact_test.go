package model

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

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
