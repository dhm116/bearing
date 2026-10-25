package github

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

func TestParseManifests(t *testing.T) {
	tests := []struct {
		name    string
		parse   func(string) ([]runtime, error)
		text    string
		want    []runtime
		wantErr bool
	}{
		{"go directive", parseGoMod, "module x\n\ngo 1.21 // old\n", []runtime{{"go", "1.21", ""}}, false},
		{"go newer toolchain", parseGoMod, "go 1.21\ntoolchain go1.22.3\n", []runtime{{"go", "1.22.3", ""}}, false},
		{"go older toolchain", parseGoMod, "go 1.23.1\ntoolchain go1.22.3\n", []runtime{{"go", "1.23.1", ""}}, false},
		{"go missing directive", parseGoMod, "module x\n", nil, false},
		{"package.json", parsePackageJSON, `{"engines": {"node": " >=18 "}}`, []runtime{{"node", ">=18", ""}}, false},
		{"package.json no engines", parsePackageJSON, `{"name": "x"}`, nil, false},
		{"package.json malformed", parsePackageJSON, `{"engines": `, nil, true},
		{"nvmrc", fileVersion("node"), "20\n", []runtime{{"node", "20", ""}}, false},
		{"nvmrc after comments and blanks", fileVersion("node"), "# pinned\n\n  lts/hydrogen  \n18\n", []runtime{{"node", "lts/hydrogen", ""}}, false},
		{"python-version first of several", fileVersion("python"), "3.12.1\n3.11\n", []runtime{{"python", "3.12.1", ""}}, false},
		{"single-value file empty", fileVersion("node"), "\n# none\n", nil, false},
		{"single-value file too long", fileVersion("node"), strings.Repeat("1", 65), nil, true},
		{"single-value file not a version", fileVersion("node"), "use \"node 20\"\n", nil, true},
		{"single-value file with a control character", fileVersion("python"), "3.12\x1b[31m\n", nil, true},
		{"pyproject", parsePyproject, "[build-system]\nrequires-python = \"<3\"\n[project]\nname = \"x\"\nrequires-python = \">=3.10\"\n", []runtime{{"python", ">=3.10", ""}}, false},
		{"pyproject single quotes", parsePyproject, "[project]\nrequires-python = '>=3.9'\n", []runtime{{"python", ">=3.9", ""}}, false},
		{"pyproject missing field", parsePyproject, "[project]\nname = \"x\"\n", nil, false},
		{"pyproject malformed", parsePyproject, "[project]\nrequires-python = 3.10\n", nil, true},
		{
			"tool-versions", parseToolVersions, "# tools\nnodejs 20.1.0 18.0.0\npython 3.12 # latest\n\nbad\n",
			[]runtime{{"nodejs", "20.1.0", ""}, {"python", "3.12", ""}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.parse(tt.text)
			if (err != nil) != tt.wantErr || !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, %v; want %v, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestSyncEmitsRuntimeClaims(t *testing.T) {
	r := newRig(t)
	obs, _ := r.sync()
	got := map[string][]string{}
	for _, o := range obs {
		e := o.GetData().GetEntity()
		if rt, ok := e.GetAttributes()["runtime"]; ok {
			got[e.GetAliases()[0]] = nil
			for _, v := range rt.GetListValue().GetValues() {
				f := v.GetStructValue().GetFields()
				got[e.GetAliases()[0]] = append(got[e.GetAliases()[0]],
					f["language"].GetStringValue()+" "+f["version"].GetStringValue()+" "+f["file"].GetStringValue())
			}
		}
	}
	want := map[string][]string{
		"github:repo/acme/payments-api": {"go 1.21 go.mod"},
		"github:repo/acme/web":          {"node >=18 package.json", "node 20 .nvmrc"},
		"github:repo/acme/handbook":     {"node lts/hydrogen .nvmrc"},
		"github:repo/acme/ops-scripts":  nil, // an empty set, so a removed file closes its fact
		"github:repo/acme/sandbox":      {"go 1.23.1 go.mod"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("%s: got %v, want %v", k, got[k], w)
		}
	}
}

func TestRuntimesSkipUnparsableAndLeaveUnreadableAlone(t *testing.T) {
	c := &client{}
	text := `{"engines": `
	r := gqlRepo{Manifests: map[string]*gqlBlob{"packageJSON": {Typename: "Blob", Text: &text}}}
	v, ok := c.runtimes(t.Context(), r)
	if !ok || len(v.GetListValue().GetValues()) != 0 {
		t.Errorf("malformed file: got %v, %v; want an empty set", v, ok)
	}
	r.Manifests["packageJSON"] = &gqlBlob{Typename: "Blob", IsBinary: true}
	if v, ok := c.runtimes(t.Context(), r); ok || v != (*structpb.Value)(nil) {
		t.Errorf("unreadable file: got %v, %v; want runtime left alone", v, ok)
	}
}
