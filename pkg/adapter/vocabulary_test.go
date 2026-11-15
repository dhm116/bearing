package adapter

import (
	"context"
	"reflect"
	"testing"

	"bearing.example/pkg/vocab"
)

// publisher claims to be the core and to be verified, to see what survives.
type publisher struct{ pager }

func (publisher) Describe(context.Context) (DescribeResult, error) {
	return DescribeResult{Name: "pub", Version: "1", Vocabulary: &vocab.Module{
		Owner: vocab.OwnerCore, Namespace: "io.example", Version: "0.4.0",
		Source: vocab.Source{Repo: "example/repo", Path: "vocab.yaml", Ref: "abc", VerifiedBy: "build"},
		Terms: []vocab.Term{
			{Name: "Widget", Is: vocab.IsKind},
			{
				Name: "size", Is: vocab.IsAttribute, Type: "float", Cardinality: "one", Conflict: "none", Domain: []string{"Widget"},
				Deprecated: &vocab.Deprecation{Since: "0.4.0", ReplacedBy: "io.example:dim"},
			},
		},
		Suggests: []vocab.Mapping{{ID: "w", Subject: "io.example:Widget", Predicate: vocab.Broader, Object: vocab.CoreNamespace + ":Component", Status: vocab.Asserted}},
	}}, nil
}

func TestDescribeOverTheProtocolCarriesTheModuleButNotItsOwner(t *testing.T) {
	t.Parallel()
	r, err := Dial("pub-source", DialConfig{Address: serveGRPC(t, publisher{})})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.Describe(t.Context())
	if err != nil || d.Vocabulary == nil {
		t.Fatalf("got %+v, %v, want a module", d, err)
	}
	want, _ := publisher{}.Describe(t.Context())
	w := *want.Vocabulary
	// What the core decides, whatever the adapter said.
	w.Owner, w.Source.VerifiedBy = vocab.OwnerAdapter, "adapter:pub-source"
	w.Suggests = []vocab.Mapping{{ID: "w", Subject: "io.example:Widget", Predicate: vocab.Broader, Object: vocab.CoreNamespace + ":Component", Status: vocab.Proposed}}
	if !reflect.DeepEqual(*d.Vocabulary, w) {
		t.Errorf("got %+v, want %+v", *d.Vocabulary, w)
	}
}
