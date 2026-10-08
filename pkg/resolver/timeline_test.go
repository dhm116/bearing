package resolver

import (
	"fmt"
	"testing"

	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// okey is an ordering key at day d with a distinguishing observation ID.
func okey(d int, id string) *resolverv1alpha1.OrderingKey {
	return model.NewOrderingKey(ts(day(d)), id, "ev-"+id, "h")
}

func TestAddWriteKeepsOnlyRelevantWrites(t *testing.T) {
	tests := []struct {
		name string
		have []write
		add  write
		want []string // "subject@day" in order
		same bool     // the list is unchanged
	}{
		{
			name: "a later start with a lesser key is irrelevant",
			have: []write{{key: okey(5, "a"), from: ts(day(5)), subject: "S1"}},
			add:  write{key: okey(3, "b"), from: ts(day(6)), subject: "S2"},
			want: []string{"S1@5"}, same: true,
		},
		{
			name: "an earlier start with a greater key replaces",
			have: []write{{key: okey(3, "a"), from: ts(day(5)), subject: "S1"}},
			add:  write{key: okey(6, "b"), from: ts(day(4)), subject: "S2"},
			want: []string{"S2@4"},
		},
		{
			name: "a later start with a greater key adds",
			have: []write{{key: okey(3, "a"), from: ts(day(3)), subject: "S1"}},
			add:  write{key: okey(6, "b"), from: ts(day(6)), subject: "S2"},
			want: []string{"S1@3", "S2@6"},
		},
		{
			name: "the same write again changes nothing",
			have: []write{{key: okey(3, "a"), from: ts(day(3)), subject: "S1"}},
			add:  write{key: okey(3, "a"), from: ts(day(3)), subject: "S1"},
			want: []string{"S1@3"}, same: true,
		},
		{
			name: "a tentative write is added alongside",
			have: []write{{key: okey(3, "a"), from: ts(day(3)), subject: "S1"}},
			add:  write{key: okey(4, "b"), subject: "P", tentative: true},
			want: []string{"S1@3", "P@0"},
		},
		{
			name: "a tentative write to the same subject keeps the greater key",
			have: []write{{key: okey(4, "a"), subject: "P", tentative: true}},
			add:  write{key: okey(3, "b"), subject: "P", tentative: true},
			want: []string{"P@0"}, same: true,
		},
		{
			name: "a later reference moves a tentative write forward",
			have: []write{{key: okey(4, "a"), subject: "P", tentative: true}},
			add:  write{key: okey(7, "b"), subject: "P", tentative: true},
			want: []string{"P@0"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := addWrite(tc.have, tc.add)
			if changed == tc.same {
				t.Fatalf("changed = %v, want %v", changed, !tc.same)
			}
			var lines []string
			for _, w := range got {
				d := 0
				if !w.tentative {
					d = int(w.from.Sub(ts(day(1))).Hours()/24) + 1
				}
				lines = append(lines, fmt.Sprintf("%s@%d", w.subject, d))
			}
			if fmt.Sprint(lines) != fmt.Sprint(tc.want) {
				t.Fatalf("got %v, want %v", lines, tc.want)
			}
		})
	}
}

func TestAddMarkKeepsOnlyRelevantMarks(t *testing.T) {
	ms, changed := addMark(nil, mark{at: ts(day(5)), key: okey(5, "a")})
	if !changed || len(ms) != 1 {
		t.Fatalf("got %v changed=%v, want one mark", ms, changed)
	}
	// A later deletion with a lesser key is already covered.
	if got, changed := addMark(ms, mark{at: ts(day(6)), key: okey(4, "b")}); changed || len(got) != 1 {
		t.Fatalf("got %v changed=%v, want the list unchanged", got, changed)
	}
	// An earlier deletion with a greater key replaces it.
	got, changed := addMark(ms, mark{at: ts(day(4)), key: okey(7, "c")})
	if !changed || len(got) != 1 || !got[0].at.Equal(ts(day(4))) {
		t.Fatalf("got %v changed=%v, want only the earlier, greater mark", got, changed)
	}
	// Later with a greater key: both matter.
	if got, changed := addMark(ms, mark{at: ts(day(8)), key: okey(9, "d")}); !changed || len(got) != 2 {
		t.Fatalf("got %v changed=%v, want two marks", got, changed)
	}
}

// bindingRows on a few hand-built timelines: the rules in the data model's
// "Bindings" section, one by one.
func TestBindingRowsApplyTheBindingRules(t *testing.T) {
	noMarks := func(string, string) []mark { return nil }
	same := func(s string) string { return s }
	name := func(alias string, perSubject, redirects bool, ws ...write) *nameWrites {
		return &nameWrites{alias: model.Key(alias), ns: "github", perSubject: perSubject, redirects: redirects, writes: ws}
	}
	show := func(rows map[model.Key][]string) string { return fmt.Sprint(rows) }
	render := func(names []*nameWrites, marks marksFor) map[model.Key][]string {
		out := map[model.Key][]string{}
		for a, bs := range bindingRows(names, same, marks) {
			for _, b := range bs {
				out[a] = append(out[a], fmt.Sprintf("%s [%s,%s) %s rel=%t tent=%t", a, tstr(b.GetValidFrom().AsTime(), b.ValidFrom != nil),
					tstr(b.GetValidTo().AsTime(), b.ValidTo != nil), b.GetSubjectId(), b.GetReleased(), b.GetTentative()))
			}
		}
		return out
	}

	t.Run("a name is back-extended to before its first write", func(t *testing.T) {
		got := render([]*nameWrites{name("github:team/acme/a", false, false, write{key: okey(3, "a"), from: ts(day(3)), subject: "S1"})}, noMarks)
		if want := "map[github:team/acme/a:[github:team/acme/a [*,*) S1 rel=false tent=false]]"; show(got) != want {
			t.Fatalf("got %s, want %s", show(got), want)
		}
	})
	t.Run("the greatest key wins from its start on", func(t *testing.T) {
		got := render([]*nameWrites{name("github:team/acme/a", false, false,
			write{key: okey(3, "a"), from: ts(day(3)), subject: "S1"},
			write{key: okey(6, "b"), from: ts(day(6)), subject: "S2"})}, noMarks)
		if n := len(got["github:team/acme/a"]); n != 2 {
			t.Fatalf("got %v, want two rows", got)
		}
	})
	t.Run("a per_subject: one subject holds only the name with the greatest key", func(t *testing.T) {
		got := render([]*nameWrites{
			name("github:repo/acme/old", true, true, write{key: okey(1, "a"), from: ts(day(1)), subject: "S1"}),
			name("github:repo/acme/new", true, true, write{key: okey(5, "b"), from: ts(day(5)), subject: "S1"}),
		}, noMarks)
		old := got["github:repo/acme/old"]
		if len(old) != 2 || old[1] != "github:repo/acme/old [10-05T00,*) S1 rel=true tent=false" {
			t.Fatalf("old name: got %v, want bound until day 5, then released with a redirect", old)
		}
		// Back-extension never releases a name, and is dropped only where
		// the subject holds another name by a covering write: before day 1
		// neither name is held by a covering write, so both reach back.
		if nw := got["github:repo/acme/new"]; len(nw) != 2 || nw[0] != "github:repo/acme/new [*,10-01T00) S1 rel=false tent=false" ||
			nw[1] != "github:repo/acme/new [10-05T00,*) S1 rel=false tent=false" {
			t.Fatalf("new name: got %v, want back-extended to before day 1, not held from day 1 to 5, bound from day 5", nw)
		}
	})
	t.Run("a release without a redirect names no subject", func(t *testing.T) {
		got := render([]*nameWrites{
			name("github:team/acme/old", true, false, write{key: okey(1, "a"), from: ts(day(1)), subject: "S1"}),
			name("github:team/acme/new", true, false, write{key: okey(5, "b"), from: ts(day(5)), subject: "S1"}),
		}, noMarks)
		if old := got["github:team/acme/old"]; len(old) != 2 || old[1] != "github:team/acme/old [10-05T00,*)  rel=true tent=false" {
			t.Fatalf("old name: got %v", old)
		}
	})
	t.Run("a deletion releases a name unless a greater key rebinds it", func(t *testing.T) {
		marks := func(ns, subject string) []mark {
			if subject == "S1" {
				return []mark{{at: ts(day(4)), key: okey(4, "del")}}
			}
			return nil
		}
		got := render([]*nameWrites{name("github:team/acme/a", false, false,
			write{key: okey(1, "a"), from: ts(day(1)), subject: "S1"},
			write{key: okey(6, "b"), from: ts(day(6)), subject: "S2"})}, marks)
		rows := got["github:team/acme/a"]
		if len(rows) != 3 || rows[1] != "github:team/acme/a [10-04T00,10-06T00)  rel=true tent=false" {
			t.Fatalf("got %v, want bound, released from the deletion, then rebound", rows)
		}
	})
	t.Run("a tentative write fills what is released without redirect", func(t *testing.T) {
		got := render([]*nameWrites{name("github:team/acme/a", true, false,
			write{key: okey(1, "a"), from: ts(day(5)), subject: "S1"},
			write{key: okey(2, "t"), subject: "P", tentative: true})}, func(ns, subject string) []mark {
			return []mark{{at: ts(day(7)), key: okey(7, "del")}}
		})
		rows := got["github:team/acme/a"]
		if len(rows) != 2 || rows[0] != "github:team/acme/a [*,10-07T00) S1 rel=false tent=false" ||
			rows[1] != "github:team/acme/a [10-07T00,*) P rel=false tent=true" {
			t.Fatalf("got %v, want the placeholder only after the name is released", rows)
		}
	})
}

func TestFoldAndEscape(t *testing.T) {
	for in, want := range map[string]string{"ACME": "acme", "Acme/Pay": "acme/pay", "straße": "straße", "ǅ": "ǆ", "": ""} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"plain": "plain", "a/b": "a%2Fb", "a:b": "a%3Ab", "100%": "100%25", "%2F": "%252F"} {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}
