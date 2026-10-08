package github

import "strings"

// rule is one rule line of a CODEOWNERS file.
type rule struct {
	line    int // 1-based
	pattern string
	owners  []string // as written: @user, @org/team or an email address
}

// codeownersPaths are where GitHub looks for CODEOWNERS, in its own order.
// The first file found is the effective one; the others are ignored.
var codeownersPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// parseCodeowners returns the rule lines of a CODEOWNERS file, including
// lines that name no owner (they un-own a path). Blank lines, comments and
// section headers ("[Docs]", "^[Docs]") are not rules.
func parseCodeowners(text string) []rule {
	var rules []rule
	for i, line := range strings.Split(text, "\n") {
		fields := strings.Fields(stripComment(line))
		if len(fields) == 0 || strings.HasPrefix(fields[0], "[") || strings.HasPrefix(fields[0], "^[") {
			continue
		}
		rules = append(rules, rule{line: i + 1, pattern: fields[0], owners: fields[1:]})
	}
	return rules
}

// stripComment removes a trailing comment: "#" starts one unless escaped.
func stripComment(line string) string {
	for i := range len(line) {
		if line[i] == '#' && (i == 0 || line[i-1] != '\\') {
			return line[:i]
		}
	}
	return line
}

// ownerKey maps an owner to a key in namespace. Email owners return false:
// they don't map to a GitHub account.
func ownerKey(namespace, owner string) (string, bool) {
	name, ok := strings.CutPrefix(owner, "@")
	if !ok || name == "" {
		return "", false
	}
	if org, slug, isTeam := strings.Cut(name, "/"); isTeam {
		return string(teamName(namespace, org, slug)), true
	}
	return string(userName(namespace, name)), true
}
