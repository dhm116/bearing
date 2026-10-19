// Command reviewcard posts and updates a pull request's review-round comment
// (docs/review-process.md, "Review comment"). The comment shows the round as
// a picture of one card per reviewer, in light and dark, with the findings
// tables under it.
//
// The first run for a round hides the lead's earlier review comments on the
// PR (minimized as outdated, not deleted) and posts the round's comment. Each
// later run for the same round edits that comment. gh uploads images only
// through `gh pr comment --attach`, and can add them to an existing comment
// only by editing the caller's latest one (--edit-last), so a run refuses when
// the round's comment is no longer the latest; runs must go one at a time. gh
// appends the uploads as Markdown images; the run then rewrites the comment so
// the <picture> points at them. Once every reviewer is done, a run needs
// -details, so the final verdict and the findings tables go out together.
//
// Usage, from the repository root (paths resolve against tools/, so give
// absolute ones):
//
//	go -C tools run ./reviewcard -pr 292 -round /tmp/round.json [-details /tmp/findings.md]
//	go -C tools run ./reviewcard -round /tmp/round.json -out /tmp/cards   # render only
//
// round.json names the round and each reviewer's state: queued, running,
// approve, discuss or changes. Every round lists ponytail, which runs last.
//
//	{"round": 2, "reviewers": [
//	  {"name": "security", "state": "changes", "suggestions": 3,
//	   "blockers": ["Webhook body is read before the size cap"]},
//	  {"name": "go", "state": "running"},
//	  {"name": "ponytail", "state": "queued"}]}
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type round struct {
	Round     int        `json:"round"`
	Reviewers []reviewer `json:"reviewers"`
}

type reviewer struct {
	Name        string   `json:"name"` // "security" or "security-reviewer"
	State       string   `json:"state"`
	Blockers    []string `json:"blockers,omitempty"` // a few words each
	Suggestions int      `json:"suggestions,omitempty"`
}

// comment is an issue comment on the PR, as the list call below shapes it.
type comment struct {
	ID     int64  `json:"id"`
	NodeID string `json:"node_id"`
	Login  string `json:"login"`
	Body   string `json:"body"`
}

// ghFunc runs the GitHub CLI with args and returns its standard output.
type ghFunc func(args ...string) ([]byte, error)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, gh))
}

func gh(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func run(args []string, stdout, stderr io.Writer, gh ghFunc) int {
	fs := flag.NewFlagSet("reviewcard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pr := fs.Int("pr", 0, "pull request number")
	roundFile := fs.String("round", "", "JSON file with the round number and each reviewer's state")
	detailsFile := fs.String("details", "", "Markdown placed under the cards: the findings tables, required once every reviewer is done")
	out := fs.String("out", "", "write the cards and the comment body to this directory instead of posting")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *roundFile == "" || (*pr <= 0) == (*out == "") {
		_, _ = fmt.Fprintln(stderr, "reviewcard: need -round and exactly one of -pr or -out")
		return 2
	}
	rd, err := loadRound(*roundFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewcard: %v\n", err)
		return 1
	}
	var details string
	if *detailsFile != "" {
		b, err := os.ReadFile(*detailsFile)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "reviewcard: %v\n", err)
			return 1
		}
		details = string(b)
	}
	if *out != "" {
		if err := os.MkdirAll(*out, 0o750); err != nil {
			_, _ = fmt.Fprintf(stderr, "reviewcard: %v\n", err)
			return 1
		}
		light, dark, err := writeCards(*out, rd)
		if err == nil {
			err = os.WriteFile(filepath.Join(*out, "body.md"), []byte(body(rd, "./"+filepath.Base(light), "./"+filepath.Base(dark), details)), 0o600) //nolint:gosec // G703: the flag names the directory to write, as for any command line tool
		}
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "reviewcard: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, *out)
		return 0
	}
	if overall(rd.Reviewers) != "running" && strings.TrimSpace(details) == "" {
		_, _ = fmt.Fprintln(stderr, "reviewcard: every reviewer is done, so pass the findings tables with -details")
		return 2
	}
	dir, err := os.MkdirTemp("", "reviewcard-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewcard: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	id, err := post(gh, *pr, rd, details, dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewcard: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "#issuecomment-%d\n", id)
	return 0
}

func loadRound(path string) (round, error) {
	var rd round
	b, err := os.ReadFile(path)
	if err != nil {
		return rd, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rd); err != nil {
		return rd, fmt.Errorf("%s: %w", path, err)
	}
	if rd.Round < 1 {
		return rd, fmt.Errorf("%s: round must be 1 or more", path)
	}
	if len(rd.Reviewers) == 0 {
		return rd, fmt.Errorf("%s: no reviewers", path)
	}
	for i := range rd.Reviewers {
		r := &rd.Reviewers[i]
		r.Name = strings.TrimSuffix(r.Name, "-reviewer")
		if _, ok := personas[r.Name]; !ok {
			return rd, fmt.Errorf("%s: unknown reviewer %q", path, r.Name)
		}
		switch r.State {
		case "queued", "running":
			if len(r.Blockers) > 0 || r.Suggestions > 0 {
				return rd, fmt.Errorf("%s: %s is %s but has findings", path, r.Name, r.State)
			}
		case "approve", "discuss", "changes":
		default:
			return rd, fmt.Errorf("%s: %s: unknown state %q", path, r.Name, r.State)
		}
		if r.Suggestions < 0 {
			return rd, fmt.Errorf("%s: %s: negative suggestions", path, r.Name)
		}
		if r.State == "approve" && len(r.Blockers) > 0 {
			return rd, fmt.Errorf("%s: %s approves with blocking findings", path, r.Name)
		}
		for _, b := range r.Blockers {
			// A line break ends GitHub's HTML block mid-<picture>; other
			// control characters make the SVG invalid.
			if strings.IndexFunc(b, func(c rune) bool { return c < 0x20 || c == 0x7f }) >= 0 {
				return rd, fmt.Errorf("%s: %s: blocking finding %q has a line break or control character", path, r.Name, b)
			}
		}
	}
	// Every round ends with the ponytail review (docs/review-process.md, "Rounds").
	if !slices.ContainsFunc(rd.Reviewers, func(r reviewer) bool { return r.Name == "ponytail" }) {
		return rd, fmt.Errorf("%s: no ponytail reviewer; list it, queued until the others finish", path)
	}
	return rd, nil
}

func marker(n int) string { return fmt.Sprintf("<!-- bearing-review round=%d -->", n) }

// isReview reports whether body is a review comment: a round's card comment,
// or one in the format used before the cards.
func isReview(body string) bool {
	return strings.Contains(body, "<!-- bearing-review round=") || strings.HasPrefix(strings.TrimSpace(body), "## Code Review:")
}

func body(rd round, light, dark, details string) string {
	b := fmt.Sprintf("%s\n<picture>\n  <source media=\"(prefers-color-scheme: dark)\" srcset=\"%s\">\n  <img src=\"%s\" alt=\"%s\" width=\"%d\">\n</picture>\n",
		marker(rd.Round), dark, light, esc(alt(rd)), width)
	if d := strings.TrimSpace(details); d != "" {
		b += "\n" + d + "\n"
	}
	return b
}

// writeCards writes the round's light and dark SVGs into dir. post names each
// upload after its file, which is how it finds them again.
func writeCards(dir string, rd round) (light, dark string, err error) {
	paths := make([]string, len(themes))
	for i, t := range themes {
		paths[i] = filepath.Join(dir, fmt.Sprintf("review-round-%d-%s.svg", rd.Round, t.name))
		if err := os.WriteFile(paths[i], []byte(svg(t, rd)), 0o600); err != nil {
			return "", "", err
		}
	}
	return paths[0], paths[1], nil
}

func listComments(gh ghFunc, pr int) ([]comment, error) {
	out, err := gh("api", "--paginate", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", pr),
		"--jq", ".[] | {id, node_id, login: .user.login, body}")
	if err != nil {
		return nil, err
	}
	var cs []comment
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var c comment
		if err := dec.Decode(&c); errors.Is(err, io.EOF) {
			return cs, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode comments: %w", err)
		}
		cs = append(cs, c)
	}
}

// assetURL finds the URL gh gave the upload named name, from the Markdown
// image gh appended to the comment with that name as its alt text.
func assetURL(body, name string) (string, error) {
	re := regexp.MustCompile(`!\[` + regexp.QuoteMeta(name) + `\]\((https://github\.com/user-attachments/assets/[^)\s]+)\)`)
	m := re.FindAllStringSubmatch(body, -1)
	if len(m) == 0 {
		return "", fmt.Errorf("no uploaded %s in the comment", name)
	}
	return m[len(m)-1][1], nil
}

// post creates or updates the round's comment on pr and returns its ID.
func post(gh ghFunc, pr int, rd round, details, dir string) (int64, error) {
	login, err := gh("api", "user", "--jq", ".login")
	if err != nil {
		return 0, err
	}
	me := strings.TrimSpace(string(login))
	cs, err := listComments(gh, pr)
	if err != nil {
		return 0, err
	}
	var mine []comment
	var currentID int64 // the round's comment, 0 when the round is new
	for _, c := range cs {
		if c.Login != me {
			continue
		}
		mine = append(mine, c)
		if strings.Contains(c.Body, marker(rd.Round)) {
			currentID = c.ID
		}
	}
	light, dark, err := writeCards(dir, rd)
	if err != nil {
		return 0, err
	}
	bodyFile := filepath.Join(dir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body(rd, "./"+filepath.Base(light), "./"+filepath.Base(dark), details)), 0o600); err != nil { //nolint:gosec // G703: dir is this run's own temporary directory
		return 0, err
	}
	// Name each upload ourselves: the read-back below finds it by that name.
	name := func(p string) string { return strings.TrimSuffix(filepath.Base(p), ".svg") }
	args := []string{"pr", "comment", strconv.Itoa(pr), "--body-file", bodyFile, "--attach", light + "#" + name(light), "--attach", dark + "#" + name(dark)}
	if currentID != 0 {
		if last := mine[len(mine)-1]; last.ID != currentID {
			return 0, fmt.Errorf("round %d's comment %d is not your latest on PR #%d (%d is), so gh would edit the wrong one; post other notes after the round", rd.Round, currentID, pr, last.ID)
		}
		args = append(args, "--edit-last")
	}
	if _, err := gh(args...); err != nil {
		return 0, err
	}
	// Hide the earlier rounds only after the post, so a failed post leaves
	// them showing. Every run hides them again (hiding a hidden comment
	// succeeds), so a run that failed here is repaired by the next.
	for _, c := range mine {
		if c.ID != currentID && isReview(c.Body) {
			if _, err := gh("api", "graphql", "-f", "query=mutation($id:ID!){minimizeComment(input:{subjectId:$id,classifier:OUTDATED}){clientMutationId}}", "-f", "id="+c.NodeID); err != nil {
				return 0, fmt.Errorf("hide comment %d: %w", c.ID, err)
			}
		}
	}
	// Read the comment back to learn the upload URLs gh appended to it.
	cs, err = listComments(gh, pr)
	if err != nil {
		return 0, err
	}
	var target *comment
	for i := range cs {
		c := &cs[i]
		if c.Login == me && strings.Contains(c.Body, marker(rd.Round)) && (currentID == 0 || c.ID == currentID) {
			target = c
		}
	}
	if target == nil {
		return 0, fmt.Errorf("round %d's comment not found on PR #%d after posting", rd.Round, pr)
	}
	lightURL, err := assetURL(target.Body, name(light))
	if err != nil {
		return 0, err
	}
	darkURL, err := assetURL(target.Body, name(dark))
	if err != nil {
		return 0, err
	}
	final := filepath.Join(dir, "final.md")
	if err := os.WriteFile(final, []byte(body(rd, lightURL, darkURL, details)), 0o600); err != nil { //nolint:gosec // G703: dir is this run's own temporary directory
		return 0, err
	}
	if _, err := gh("api", "-X", "PATCH", fmt.Sprintf("repos/{owner}/{repo}/issues/comments/%d", target.ID), "-F", "body=@"+final); err != nil {
		return 0, err
	}
	return target.ID, nil
}
