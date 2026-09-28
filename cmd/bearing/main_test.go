package main

import (
	"os"
	"strings"
	"testing"
)

func TestValidateExampleFile(t *testing.T) {
	f, err := os.Open("../../testdata/observations.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := validate(f)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("validated %d observations, want 4", n)
	}
}

func TestValidateReportsLineNumbers(t *testing.T) {
	in := "{\"specversion\":\"1.0\"}\n\nnot json\n"
	_, err := validate(strings.NewReader(in))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"line 1:", "line 3:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
