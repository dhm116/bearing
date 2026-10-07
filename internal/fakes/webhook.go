package fakes

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G505: git object IDs are SHA-1 by definition
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// Delivery is a webhook delivery GitHub would send for a change to the org.
type Delivery struct {
	// ID is the X-GitHub-Delivery GUID, deterministic per change.
	ID string
	// Event is the X-GitHub-Event name: repository, push, team or
	// membership.
	Event string
	Body  []byte
	// Signature is X-Hub-Signature-256, or "" without a webhook secret.
	Signature string
}

// Header returns the delivery's HTTP headers.
func (d Delivery) Header() http.Header {
	h := http.Header{
		"Content-Type":      {"application/json"},
		"User-Agent":        {"GitHub-Hookshot/fake"},
		"X-Github-Event":    {d.Event},
		"X-Github-Delivery": {d.ID},
	}
	if d.Signature != "" {
		h.Set("X-Hub-Signature-256", d.Signature)
	}
	return h
}

// Request returns the delivery as a POST to url.
func (d Delivery) Request(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(d.Body))
	if err != nil {
		return nil, err
	}
	req.Header = d.Header()
	return req, nil
}

// Sign returns GitHub's X-Hub-Signature-256 value for body: "sha256=" and
// the hex HMAC-SHA256 of body keyed with secret.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Deliveries returns a delivery for each GitHub change made to the org so
// far, in order, signed with the webhook secret. Changes GitHub can't see
// (a membership of someone without a GitHub account) have none.
func (g *GitHub) Deliveries() []Delivery {
	g.org.mu.Lock()
	defer g.org.mu.Unlock()
	var out []Delivery
	for i, c := range g.org.changes {
		event, payload := g.org.webhookPayload(c)
		if payload == nil {
			continue
		}
		payload["organization"] = orgJSON(githubAPI, true)
		payload["sender"] = map[string]any{"login": "acme-admin", "id": 1000001, "type": "User"}
		body, err := json.Marshal(payload)
		if err != nil {
			panic(err) // maps of strings, numbers and bools always marshal
		}
		d := Delivery{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), Event: event, Body: body}
		if g.secret != "" {
			d.Signature = Sign(g.secret, body)
		}
		out = append(out, d)
	}
	return out
}

// webhookPayload renders c as GitHub would, with next-format node IDs: a
// webhook request carries no header to choose them. The caller holds o.mu.
func (o *Org) webhookPayload(c change) (string, map[string]any) {
	switch c.kind {
	case repoRenamed:
		return "repository", map[string]any{
			"action":     "renamed",
			"changes":    map[string]any{"repository": map[string]any{"name": map[string]any{"from": c.from}}},
			"repository": repoJSON(githubAPI, c.repo, true),
		}
	case filePushed:
		commit := map[string]any{
			"id": commitSHA(c), "message": "Update " + c.path, "timestamp": ghTime(c.at),
			"added": []string{}, "removed": []string{}, "modified": []string{},
		}
		commit[c.op] = []string{c.path}
		return "push", map[string]any{
			"ref": "refs/heads/" + c.repo.DefaultBranch, "before": parentSHA(c), "after": commitSHA(c),
			"created": false, "deleted": false, "forced": false,
			"commits": []any{commit}, "head_commit": commit,
			"repository": repoJSON(githubAPI, c.repo, true),
			"pusher":     map[string]any{"name": "acme-admin", "email": "admin@acme.example"},
		}
	case teamRenamed, teamDeleted:
		if c.team.GitHub == nil {
			return "", nil
		}
		p := map[string]any{"action": "deleted", "team": o.teamJSON(githubAPI, c.team, true)}
		if c.kind == teamRenamed {
			p["action"] = "edited"
			p["changes"] = map[string]any{"name": map[string]any{"from": c.from}}
		}
		return "team", p
	default: // memberAdded, memberRemoved
		if c.person.GitHub == nil || c.team.GitHub == nil {
			return "", nil
		}
		action := "added"
		if c.kind == memberRemoved {
			action = "removed"
		}
		return "membership", map[string]any{
			"action": action, "scope": "team",
			"member": userJSON(githubAPI, *c.person.GitHub, true),
			"team":   o.teamJSON(githubAPI, c.team, true),
		}
	}
}

// commitSHA is a stable fake commit ID for a push.
func commitSHA(c change) string {
	return gitSHA("commit", c.repo.Name+"\x00"+c.path+"\x00"+c.at.String())
}

func parentSHA(c change) string { return gitSHA("commit", "parent\x00"+commitSHA(c)) }

// blobSHA is a file's git blob ID, as the contents API reports it.
func blobSHA(content string) string { return gitSHA("blob", content) }

func gitSHA(kind, content string) string {
	h := sha1.New() //nolint:gosec // G401: git object IDs are SHA-1 by definition
	h.Write([]byte(kind + " " + strconv.Itoa(len(content)) + "\x00" + content))
	return hex.EncodeToString(h.Sum(nil))
}
