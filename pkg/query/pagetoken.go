package query

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"bearing.example/pkg/contracts"
)

// maxPageToken bounds the token a caller may hand back: the question it
// carries is a subject ID, two times and a cursor.
const maxPageToken = 512

// tokenVersion is the version of the token format. A token of another
// version is refused rather than guessed at.
const tokenVersion = 1

// changesState is a Changes question once its defaults are resolved: what a
// page token carries, so that every page of an answer is read on the same
// window and subject.
type changesState struct {
	axis         Axis
	since, until time.Time
	deflt        bool
	// subject is the canonical subject ID, or empty for the whole graph.
	subject string
	// after is where the next page starts; nil on a first page.
	after *contracts.ChangeCursor
}

// tokenJSON is a page token's contents. The token is not signed or secret:
// it names the question and a position, and a server that accepts one MUST
// authorize the question it decodes to, as it would the same request made
// without a token.
type tokenJSON struct {
	V       int       `json:"v"`
	Axis    Axis      `json:"axis"`
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Default bool      `json:"default,omitempty"`
	Subject string    `json:"subject,omitempty"`
	At      time.Time `json:"changed_at"`
	FactID  string    `json:"fact_id"`
}

// encode returns the token for the page after st.after.
func (st *changesState) encode() string {
	b, err := json.Marshal(tokenJSON{
		V: tokenVersion, Axis: st.axis, Since: st.since, Until: st.until, Default: st.deflt,
		Subject: st.subject, At: st.after.ChangedAt, FactID: st.after.FactID,
	})
	if err != nil {
		// Strings and times always encode; this is not reachable.
		panic("query: encode page token: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodePageToken reads a token Changes issued.
func decodePageToken(token string) (*changesState, error) {
	if len(token) > maxPageToken {
		return nil, fmt.Errorf("%w: too long", ErrBadPageToken)
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("%w: not a token", ErrBadPageToken)
	}
	var t tokenJSON
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%w: not a token", ErrBadPageToken)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: not a token", ErrBadPageToken)
	}
	switch {
	case t.V != tokenVersion:
		return nil, fmt.Errorf("%w: version %d is not one this build reads", ErrBadPageToken, t.V)
	case t.Axis != AxisValid && t.Axis != AxisRecord:
		return nil, fmt.Errorf("%w: unknown axis", ErrBadPageToken)
	case t.Since.IsZero() || t.Since.After(t.Until) || t.Until.IsZero() || t.At.IsZero() || t.FactID == "":
		return nil, fmt.Errorf("%w: incomplete", ErrBadPageToken)
	case len(t.Subject) > 128 || len(t.FactID) > 128:
		return nil, fmt.Errorf("%w: a field is too long", ErrBadPageToken)
	}
	return &changesState{
		axis: t.Axis, since: t.Since.UTC(), until: t.Until.UTC(), deflt: t.Default, subject: t.Subject,
		after: &contracts.ChangeCursor{ChangedAt: t.At.UTC(), FactID: t.FactID},
	}, nil
}

// changesWindow resolves a request's defaults, or reads its page token.
func (q *Querier) changesWindow(req ChangesRequest) (*changesState, error) {
	if req.PageToken != "" {
		if req.Ref != "" || !req.Since.IsZero() || !req.Until.IsZero() || req.Axis != "" {
			return nil, fmt.Errorf("%w: it carries its question, so give no subject, window or axis with it", ErrBadPageToken)
		}
		return decodePageToken(req.PageToken)
	}
	st := &changesState{axis: req.Axis, since: req.Since, until: req.Until}
	if st.axis == "" {
		st.axis = AxisValid
	}
	if st.until.IsZero() {
		st.until = q.now()
	}
	if st.since.IsZero() {
		st.since = st.until.Add(-DefaultChangesWindow)
		st.deflt = req.Until.IsZero()
	}
	st.since, st.until = st.since.UTC(), st.until.UTC()
	return st, nil
}

// now is the time a question with no end is asked at.
func (q *Querier) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}
