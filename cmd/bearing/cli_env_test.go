package main

import (
	"testing"

	"bearing.example/pkg/telemetry"
)

func TestCLICommandsLogNowhereByDefaultButTheServerDoes(t *testing.T) {
	t.Parallel()
	none := func(string) string { return "" }
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"a query", []string{"owner", "x"}, nil, telemetry.ExporterNone},
		{"the server", []string{"server", "--settings", "f"}, nil, ""},
		{"a query asked to log", []string{"owner", "x"}, map[string]string{"OTEL_LOGS_EXPORTER": "console"}, "console"},
		{"a query with an OTLP endpoint", []string{"owner", "x"}, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			getenv := none
			if c.env != nil {
				getenv = func(k string) string { return c.env[k] }
			}
			if got := cliEnv(c.args, getenv)("OTEL_LOGS_EXPORTER"); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
