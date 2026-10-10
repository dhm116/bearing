package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/resolver"
	"bearing.example/pkg/store"
)

// The resolver reads the store about 18 times for each event it resolves,
// which is the time the benchmark would spend getting to 10 million facts,
// not the store's. So a Change event, which is independent of everything but
// the person who made it, is applied as the ChangeSet the resolver writes
// for it, without resolving it again. The ChangeSet is not written by hand:
// the resolver resolves one Change event once, against an in-memory store
// holding one person, and every other Change is that ChangeSet with its
// identifiers replaced. A test resolves other Changes the normal way and
// compares.
//
// The template names things by tokens that cannot occur by chance.
const (
	tokenN      = "987654321"
	tokenLogin  = "tplperson"
	tokenTimeUS = "2000-01-01T00:00:00.123456Z"
)

// changeTemplate is the resolver's ChangeSet for one Change event, as
// ProtoJSON text with tokens where the event's identity goes.
type changeTemplate struct {
	text     string
	person   string // the template person's subject ID
	hashes   [2]string
	contents string // the content hash of the template observation
	bucket   string // the quarter hour of the template's time, which names a scope's watermark entries
}

// quarterHour is the number of the quarter hour of the epoch that t is in: the
// bucket of the scope watermark entries the resolver writes for an observation
// made at t. A test compares it with the resolver's.
func quarterHour(t time.Time) string {
	return strconv.FormatInt(t.UnixMicro()/(15*60*1e6), 10)
}

var hashKey = regexp.MustCompile(`/(at|url)/=([0-9a-f]{64})`)

func newChangeTemplate(ctx context.Context, cfg resolver.Config) (*changeTemplate, error) {
	st, err := store.Open(ctx, store.Config{Graph: "mem://"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close(ctx) }()
	r, err := resolver.New(cfg, st.Graph)
	if err != nil {
		return nil, err
	}
	s := newStream(defaultOrg())
	s.people = []*personState{{id: 0, login: tokenLogin, name: "Template", email: tokenLogin + "@acme.example"}}
	at := time.Date(2000, 1, 1, 0, 0, 0, 123456000, time.UTC)
	if _, err := r.Apply(ctx, s.personEvent(s.people[0], at.Add(-time.Hour))); err != nil {
		return nil, fmt.Errorf("template person: %w", err)
	}
	ev := s.changeEvent(987654321, 0, at)
	res, err := r.Resolve(ctx, ev)
	if err != nil {
		return nil, fmt.Errorf("template change: %w", err)
	}
	if len(res.Rejections) > 0 {
		return nil, fmt.Errorf("template change rejected: %v", res.Rejections)
	}
	person, err := st.Graph.ResolveKey(ctx, model.Key(userKey(0)), at, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("template person: %w", err)
	}
	cs := res.ChangeSet
	cs.BaseRecordedAt = nil
	b, err := protojson.Marshal(cs)
	if err != nil {
		return nil, err
	}
	t := &changeTemplate{text: string(b), person: person.GetSubjectId(), bucket: quarterHour(at)}
	for _, m := range hashKey.FindAllStringSubmatch(t.text, -1) {
		switch m[1] {
		case "at":
			t.hashes[0] = m[2]
		case "url":
			t.hashes[1] = m[2]
		}
	}
	if t.hashes[0] == "" || t.hashes[1] == "" {
		return nil, fmt.Errorf("template change: no value hashes in the state keys")
	}
	h, err := model.ContentHash(ev.Observation.GetData())
	if err != nil {
		return nil, err
	}
	t.contents = h
	return t, nil
}

// build returns the ChangeSet the resolver would write for Change n, merged
// at at, by the person with the subject ID personID and the GitHub login
// login.
func (t *changeTemplate) build(n int, at time.Time, personID, login string, data *modelv1alpha1.ObservationData) (*modelv1alpha1.ChangeSet, error) {
	ts := at.UTC().Format(timeFormat)
	url := fmt.Sprintf("https://github.com/acme/r/pull/%d", n)
	atHash, err := valueHash("at", structpb.NewStringValue(ts), modelv1alpha1.ValueType_VALUE_TYPE_TIME)
	if err != nil {
		return nil, err
	}
	urlHash, err := valueHash("url", structpb.NewStringValue(url), modelv1alpha1.ValueType_VALUE_TYPE_STRING)
	if err != nil {
		return nil, err
	}
	content, err := model.ContentHash(data)
	if err != nil {
		return nil, err
	}
	r := strings.NewReplacer(
		t.hashes[0], atHash, t.hashes[1], urlHash, t.contents, content,
		t.person, personID, tokenLogin, login, tokenTimeUS, ts, tokenN, strconv.Itoa(n), t.bucket, quarterHour(at),
	)
	cs := &modelv1alpha1.ChangeSet{}
	if err := protojson.Unmarshal([]byte(r.Replace(t.text)), cs); err != nil {
		return nil, fmt.Errorf("change %d: %w", n, err)
	}
	return cs, nil
}

func valueHash(pred string, v *structpb.Value, typ modelv1alpha1.ValueType) (string, error) {
	return model.FactID("", pred, &modelv1alpha1.FactObject{Type: typ, Value: v})
}

// withBase sets the head a ChangeSet was computed from.
func withBase(cs *modelv1alpha1.ChangeSet, head time.Time) *modelv1alpha1.ChangeSet {
	if head.IsZero() {
		cs.BaseRecordedAt = nil
	} else {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	return cs
}
