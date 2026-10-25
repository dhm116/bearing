package main

import (
	"strings"
	"testing"

	"bearing.example/pkg/query"
)

func TestTextOutputDropsWhatATerminalWouldActOn(t *testing.T) {
	t.Parallel()
	var o out
	o.line(1, "%s owns %s", "team\x1b[2J", "svc\u009b31m\u202eevil\u2028x")
	o.sources(0, []query.Source{{Source: "s\x07", URL: "https://x/\x1b]0;pwn\x07"}})
	got := o.b.String()
	for _, bad := range []string{"\x1b", "\x07", "\u009b", "\u202e", "\u2028"} {
		if strings.Contains(got, bad) {
			t.Fatalf("got %q, want no %q", got, bad)
		}
	}
	if !strings.Contains(got, "  team[2J owns svc31mevilx\n") {
		t.Fatalf("got %q, want the visible text kept and one line per call", got)
	}
}
