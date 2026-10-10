package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// dumpable is what the durability tests read from a backend: the graph and
// its audit log. Both memstore and pgstore have both.
type dumpable interface {
	contracts.GraphStore
	AuditLog() contracts.AuditLog
}

// recordTimeFields are the fields of the model messages that hold when the
// store recorded something, which differs between two runs of the same
// story. Valid times (a fact's valid_from, an observation's time) come from
// the inputs and stay in the dump.
var recordTimeFields = map[string]bool{
	"recorded_at": true, "base_recorded_at": true, "retracted_at": true,
	"unmerged_at": true, "compacted_at": true, "minted_at": true,
}

// uuidV7 matches a subject ID, which carries the time and random bits of the
// apply that minted it.
var uuidV7 = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`)

// graphDump is everything a store holds, as text that two stores share when
// they hold the same graph whatever the clock said when they recorded it.
type graphDump struct {
	// Journal has one string per entry of the change journal (which is also
	// what a backup holds), in apply order. A store that stopped after j
	// applies has the first j strings of a store that went on.
	Journal []string
	// Rest is the audit records in order, the answers of the store's reads
	// for every subject in the order they were minted, and the resolver
	// state entries sorted by key.
	Rest []string
	// AuditRecords is how many audit records the dump holds, and Audited how
	// many audit entries each journal entry carries: a store that holds a
	// journal entry holds exactly its entries' records.
	AuditRecords int
	Audited      []int
}

func (d graphDump) String() string {
	return strings.Join(d.Journal, "\n") + "\n--\n" + strings.Join(d.Rest, "\n")
}

// dumpMode says how much of a store's history a dump leaves out.
type dumpMode int

const (
	// dumpExact keeps everything, so two dumps are equal only when the stores
	// hold the same bytes: the same record times, subject IDs and hashes.
	dumpExact dumpMode = iota
	// dumpCanonical leaves out what differs between two runs of the same
	// inputs that the clock and the ID source touched: record times, the
	// audit hashes (which cover them), and the times inside subject IDs.
	// Each subject ID is replaced by its number in order of appearance in the
	// journal, and so is each fact ID (the hash of its subject's ID, so it
	// differs with it). Hashes of the inputs stay.
	dumpCanonical
)

// canonicalDump reads everything the store holds: the change journal, the
// audit records, and the answers to the store's reads for every subject and
// every resolver state key the journal names.
func canonicalDump(ctx context.Context, t testing.TB, s dumpable, mode dumpMode) graphDump {
	t.Helper()
	var backup bytes.Buffer
	if err := s.Backup(ctx, &backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	br, err := contracts.NewBackupReader(&backup)
	if err != nil {
		t.Fatal(err)
	}
	var journal []string
	var audited []int
	var subjects, stateKeys []string
	for {
		rec, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		e := &modelv1alpha1.JournalEntry{}
		if err := proto.Unmarshal(rec, e); err != nil {
			t.Fatal(err)
		}
		journal = append(journal, jsonText(t, mode, e))
		audited = append(audited, len(e.GetChangeSet().GetAudit()))
		for _, m := range e.GetMinted() {
			subjects = append(subjects, m.GetSubjectId())
		}
		for _, en := range e.GetChangeSet().GetState() {
			stateKeys = append(stateKeys, en.GetKey())
		}
	}

	var rest []string
	records := 0
	for after := uint64(0); ; {
		page, err := s.AuditLog().Query(ctx, contracts.AuditFilter{After: after, Limit: contracts.MaxAuditQueryRecords})
		if err != nil {
			t.Fatalf("audit query: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if mode == dumpCanonical {
				r = proto.CloneOf(r)
				r.RecordedAt, r.PrevHash, r.Hash = nil, nil, nil
			}
			rest = append(rest, "audit "+jsonText(t, mode, r))
			after = r.GetSeq()
			records++
		}
	}

	// A group is the answer of one read for one subject, one item to a line.
	// Reads that promise no order return it by keys that hold IDs, so in a
	// canonical dump their items are sorted once numbered.
	type group struct {
		name    string
		ordered bool
		items   []string
	}
	var groups []group
	// Reads take a zero time to mean now by the store's clock, which two runs
	// do not share, so every read here names its times: as recorded at the
	// head, and as valid at the times below, one in the middle of the story
	// and one after its end.
	head, err := s.Head(ctx)
	check(t, "Head", err)
	validAt := []time.Time{time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, id := range subjects {
		sid := contracts.SubjectID(id)
		add := func(name string, ordered bool, items []string) {
			groups = append(groups, group{name: "reads " + id + " " + name, ordered: ordered, items: items})
		}
		sub, err := s.Subject(ctx, sid, head)
		check(t, "Subject "+id, err)
		add("subject", true, []string{jsonLine(t, mode, sub)})
		bindings, err := s.Bindings(ctx, nil, []contracts.SubjectID{sid}, head)
		check(t, "Bindings "+id, err)
		add("bindings", false, jsonLines(t, mode, bindings))
		merges, err := s.Merges(ctx, sid, head)
		check(t, "Merges "+id, err)
		add("merges", true, jsonLines(t, mode, merges))
		unmerges, err := s.Unmerges(ctx, sid, head)
		check(t, "Unmerges "+id, err)
		add("unmerges", true, jsonLines(t, mode, unmerges))
		supports, err := s.Supports(ctx, contracts.SupportFilter{SubjectID: sid}, head)
		check(t, "Supports "+id, err)
		add("supports", false, jsonLines(t, mode, supports))
		for _, at := range validAt {
			facts, err := s.AsOf(ctx, contracts.FactFilter{SubjectID: sid}, at, head)
			check(t, "AsOf "+id, err)
			add("facts at "+at.Format(time.DateOnly), false, jsonLines(t, mode, facts))
			conflicts, err := s.Conflicts(ctx, sid, "", at, head)
			check(t, "Conflicts "+id, err)
			add("conflicts at "+at.Format(time.DateOnly), false, jsonLines(t, mode, conflicts))
		}
	}

	var state []string
	for chunk := range slices.Chunk(slices.Compact(slices.Sorted(slices.Values(stateKeys))), 200) {
		got, err := s.State(ctx, chunk, head)
		check(t, "State", err)
		for k, v := range got {
			state = append(state, "state "+k+" "+jsonText(t, mode, v))
		}
	}

	// The subject IDs get their numbers from the journal, then the rest is
	// numbered the same way; the state entries are sorted once numbered,
	// because their keys hold subject IDs.
	n := newNumbering(mode == dumpCanonical)
	for _, texts := range [][]string{journal, rest, state} {
		for _, s := range texts {
			n.learn(s)
		}
	}
	for _, g := range groups {
		for _, s := range g.items {
			n.learn(s)
		}
	}
	for i := range journal {
		journal[i] = n.apply(journal[i])
	}
	for i := range rest {
		rest[i] = n.apply(rest[i])
	}
	for _, g := range groups {
		for i := range g.items {
			g.items[i] = n.apply(g.name + " " + g.items[i])
		}
		if !g.ordered && mode == dumpCanonical {
			n.sort(g.items)
		}
		rest = append(rest, g.items...)
	}
	for i := range state {
		state[i] = n.apply(state[i])
	}
	n.sort(state)
	rest = append(rest, state...)
	// Fact IDs are numbered last, in the order the dump now reads, because
	// the order of the items of an unordered read depends on them.
	for _, lines := range [][]string{journal, rest} {
		for i := range lines {
			lines[i] = n.number(lines[i])
		}
	}
	return graphDump{Journal: journal, Rest: rest, AuditRecords: records, Audited: audited}
}

func check(t testing.TB, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// jsonValue is m as the generic JSON value of its ProtoJSON form, without
// the record-time fields at any depth when mode is dumpCanonical.
func jsonValue(t testing.TB, mode dumpMode, m proto.Message) any {
	t.Helper()
	b, err := model.EncodeJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if mode == dumpCanonical {
		v = withoutRecordTimes(v)
	}
	return v
}

// jsonText is v, a message or a generic JSON value, as indented JSON.
func jsonText(t testing.TB, mode dumpMode, v any) string {
	t.Helper()
	if m, ok := v.(proto.Message); ok {
		v = jsonValue(t, mode, m)
	}
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jsonLine is m as one line of JSON.
func jsonLine(t testing.TB, mode dumpMode, m proto.Message) string {
	t.Helper()
	b, err := json.Marshal(jsonValue(t, mode, m))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonLines[M proto.Message](t testing.TB, mode dumpMode, ms []M) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, jsonLine(t, mode, m))
	}
	return out
}

func withoutRecordTimes(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if recordTimeFields[k] {
				delete(v, k)
				continue
			}
			v[k] = withoutRecordTimes(x)
		}
	case []any:
		for i, x := range v {
			v[i] = withoutRecordTimes(x)
		}
	}
	return v
}

// numbering gives each subject ID and each fact ID its number in order of
// first appearance. Every other hash (an observation's content hash, the
// tails of support keys) comes from the inputs and stays as it is, so a wrong
// one is a difference. Switched off, it changes nothing.
type numbering struct {
	on            bool
	subjects      map[string]string
	facts         map[string]string // the fact IDs found, and their numbers once given
	numberedFacts int
}

func newNumbering(on bool) *numbering {
	return &numbering{on: on, subjects: map[string]string{}, facts: map[string]string{}}
}

// factIDs finds the fact IDs a ProtoJSON text names: a fact's ID is the hash
// of its subject's ID, so it carries the random bits of the run that minted
// the subject.
var factIDs = regexp.MustCompile(`"fact_id":\s*"([0-9a-f]{64})"`)

// ids matches a subject ID and a 64-digit hash.
var ids = regexp.MustCompile(uuidV7.String() + `|[0-9a-f]{64}`)

// learn notes the fact IDs in s, so that number finds them wherever they
// appear (a state key may hold one).
func (n *numbering) learn(s string) {
	for _, m := range factIDs.FindAllStringSubmatch(s, -1) {
		n.facts[m[1]] = ""
	}
}

// apply replaces each subject ID in s by "subject-N".
func (n *numbering) apply(s string) string {
	if !n.on {
		return s
	}
	return ids.ReplaceAllStringFunc(s, func(id string) string {
		if len(id) == 64 {
			return id
		}
		num, ok := n.subjects[id]
		if !ok {
			num = fmt.Sprintf("subject-%d", len(n.subjects)+1)
			n.subjects[id] = num
		}
		return num
	})
}

// number replaces each fact ID in s by "fact-N", numbering new ones as it
// meets them.
func (n *numbering) number(s string) string {
	if !n.on {
		return s
	}
	return ids.ReplaceAllStringFunc(s, func(id string) string {
		num, ok := n.facts[id]
		if !ok {
			return id
		}
		if num == "" {
			n.numberedFacts++
			num = fmt.Sprintf("fact-%d", n.numberedFacts)
			n.facts[id] = num
		}
		return num
	})
}

// sort puts items in an order that does not depend on their fact IDs, or
// plainly in order when fact IDs are not numbered.
func (n *numbering) sort(items []string) {
	if !n.on {
		slices.Sort(items)
		return
	}
	mask := func(s string) string {
		return ids.ReplaceAllStringFunc(s, func(id string) string {
			if _, ok := n.facts[id]; ok {
				return "fact"
			}
			return id
		})
	}
	slices.SortStableFunc(items, func(a, b string) int { return strings.Compare(mask(a), mask(b)) })
}

// firstDifference describes where two dumps differ, for a failure message.
func firstDifference(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			lo := max(0, i-3)
			return fmt.Sprintf("line %d:\n want: %s\n  got: %s\nnear:\n%s", i+1, a[i], b[i], strings.Join(a[lo:i], "\n"))
		}
	}
	return fmt.Sprintf("one dump ends at line %d, the other at %d", len(a), len(b))
}

// verifyAuditChain fails the test unless the store's audit chain verifies
// from its first record to its head.
func verifyAuditChain(ctx context.Context, t testing.TB, s dumpable) audit.Report {
	t.Helper()
	rep, err := audit.Verify(ctx, s.AuditLog(), nil, audit.Options{})
	if err != nil {
		t.Fatalf("verify audit chain: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("audit chain does not verify: %v", rep.Failures)
	}
	// The next record chains from the head the store keeps, so it must be the
	// newest record that was verified.
	head, err := s.AuditLog().Head(ctx)
	if err != nil {
		t.Fatalf("audit head: %v", err)
	}
	if head.Seq != rep.Last {
		t.Fatalf("audit head is record %d, the chain verified up to %d", head.Seq, rep.Last)
	}
	return rep
}
