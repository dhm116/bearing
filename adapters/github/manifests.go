package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// runtime is one language version a repository declares in a manifest.
type runtime struct {
	language, version, file string
}

// manifest is a file read from the default branch for its language
// versions. alias is its GraphQL alias in repoFields.
type manifest struct {
	alias, path string
	parse       func(text string) ([]runtime, error)
}

// manifests lists the files read, in the order their claims are emitted.
// internal/fakes.manifestAliases mirrors alias and path.
var manifests = []manifest{
	{"goMod", "go.mod", parseGoMod},
	{"packageJSON", "package.json", parsePackageJSON},
	{"nvmrc", ".nvmrc", fileVersion("node")},
	{"nodeVersion", ".node-version", fileVersion("node")},
	{"pyproject", "pyproject.toml", parsePyproject},
	{"pythonVersion", ".python-version", fileVersion("python")},
	{"toolVersions", ".tool-versions", parseToolVersions},
}

// manifestFields is the part of repoFields that selects the manifests.
func manifestFields() string {
	var b strings.Builder
	for _, m := range manifests {
		fmt.Fprintf(&b, "\n        %s: object(expression: \"HEAD:%s\") { __typename ... on Blob { text isTruncated isBinary } }", m.alias, m.path)
	}
	return b.String()
}

// parseGoMod reads the go directive, or the toolchain directive when that
// names a newer Go.
func parseGoMod(text string) ([]runtime, error) {
	var goVer, toolchain string
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(stripSlashComment(line))
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "go":
			goVer = f[1]
		case "toolchain":
			toolchain = strings.TrimPrefix(f[1], "go") // "default" is not a version
		}
	}
	if goVer == "" {
		return nil, nil
	}
	if toolchain != "" && toolchain != "default" && versionLess(goVer, toolchain) {
		goVer = toolchain
	}
	return []runtime{{language: "go", version: goVer}}, nil
}

func stripSlashComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

// versionLess compares dotted numeric versions; a part that isn't a number
// counts as 0. shortcut: no pre-release ordering ("1.22rc1"), when needed
// use golang.org/x/mod/semver.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func parsePackageJSON(text string) ([]runtime, error) {
	var p struct {
		Engines struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return nil, err
	}
	if v := strings.TrimSpace(p.Engines.Node); v != "" {
		return []runtime{{language: "node", version: v}}, nil
	}
	return nil, nil
}

// fileVersion parses a file whose whole content is the version.
func fileVersion(language string) func(string) ([]runtime, error) {
	return func(text string) ([]runtime, error) {
		return []runtime{{language: language, version: strings.TrimSpace(text)}}, nil
	}
}

// parsePyproject reads project.requires-python. shortcut: a line scan of
// the [project] table, so a dotted key outside it (project.requires-python
// = ...) is missed; use a TOML parser if that shows up.
func parsePyproject(text string) ([]runtime, error) {
	inProject := false
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inProject = line == "[project]"
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !inProject || !ok || strings.TrimSpace(k) != "requires-python" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) < 2 || v[0] != v[len(v)-1] || (v[0] != '"' && v[0] != '\'') {
			return nil, fmt.Errorf("requires-python is not a string: %.40s", v)
		}
		if v = strings.TrimSpace(v[1 : len(v)-1]); v != "" {
			return []runtime{{language: "python", version: v}}, nil
		}
	}
	return nil, nil
}

// parseToolVersions reads "<tool> <version> [fallback...]" lines; the
// first version is the one asdf uses.
func parseToolVersions(text string) ([]runtime, error) {
	var out []runtime
	for line := range strings.SplitSeq(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if f := strings.Fields(line); len(f) >= 2 {
			out = append(out, runtime{language: f[0], version: f[1]})
		}
	}
	return out, nil
}
