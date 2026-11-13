package vocab

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

func TestFuncsBehaveAsSprigDoes(t *testing.T) {
	t.Parallel()
	tests := []struct{ tmpl, want string }{
		{`{{ "AbC" | lower }}`, "abc"},
		{`{{ "AbC" | upper }}`, "ABC"},
		{`{{ " a " | trim }}`, "a"},
		{`{{ "kube-api" | trimPrefix "kube-" }}`, "api"},
		{`{{ "api-svc" | trimSuffix "-svc" }}`, "api"},
		{`{{ "a-b-c" | replace "-" "_" }}`, "a_b_c"},
		{`{{ "abc" | contains "b" }}`, "true"},
		{`{{ "abc" | hasPrefix "ab" }}`, "true"},
		{`{{ "abc" | hasSuffix "bc" }}`, "true"},
		{`{{ "abcdef" | trunc 3 }}`, "abc"},
		{`{{ "ab" | trunc 3 }}`, "ab"},
		{`{{ "a\"b" | quote }}`, `"a\"b"`},
		{`{{ "a,b,c" | splitList "," | join "+" }}`, "a+b+c"},
		{`{{ "a,b,c" | splitList "," | first }}`, "a"},
		{`{{ "a,b,c" | splitList "," | last }}`, "c"},
		{`{{ "" | splitList "," | first }}{{ "" | splitList "," | last }}`, ""},
		{`{{ "" | default "x" }}`, "x"},
		{`{{ "y" | default "x" }}`, "y"},
		{`{{ "hi" | b64enc }}`, "aGk="},
		{`{{ "aGk=" | b64dec }}`, "hi"},
		{`{{ "a" | sha256sum }}`, "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"},
		{`{{ toJson . }}`, `{"a":1}`},
		{`{{ "PaymentsAPI v2" | kebabcase }}`, "payments-api-v2"},
		{`{{ "PaymentsAPI v2" | snakecase }}`, "payments_api_v2"},
		{`{{ "abc" | regexMatch "^a.c$" }}`, "true"},
		{`{{ "pay-123-x" | regexFind "[0-9]+" }}`, "123"},
		{`{{ regexReplaceAll "[0-9]+" "pay-123" "N" }}`, "pay-N"},
	}
	for _, tc := range tests {
		t.Run(tc.tmpl, func(t *testing.T) {
			t.Parallel()
			tt, err := template.New("t").Funcs(funcs).Parse(tc.tmpl)
			if err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			if err := tt.Execute(&b, map[string]any{"a": 1}); err != nil || b.String() != tc.want {
				t.Errorf("got %q, %v; want %q", b.String(), err, tc.want)
			}
		})
	}
}

func TestFuncsReportBadInput(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{`{{ "!" | b64dec }}`, `{{ "x" | regexMatch "(" }}`, `{{ "x" | regexFind "(" }}`, `{{ "x" | regexReplaceAll "(" "y" }}`, `{{ toJson .c }}`} {
		tt, err := template.New("t").Funcs(funcs).Parse(tmpl)
		if err != nil {
			t.Fatal(err)
		}
		if err := tt.Execute(&bytes.Buffer{}, map[string]any{"c": make(chan int)}); err == nil {
			t.Errorf("%s: no error", tmpl)
		}
	}
}

func TestFuncsHaveNoImpureOnes(t *testing.T) {
	t.Parallel()
	for name := range funcs {
		if strings.Contains(strings.ToLower(name), "env") || strings.Contains(name, "now") || strings.Contains(name, "rand") || strings.Contains(name, "uuid") {
			t.Errorf("%s is not deterministic", name)
		}
	}
}
