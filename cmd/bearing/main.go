// Command bearing is the Bearing developer CLI. It helps adapter authors run
// and check adapters:
//
//	bearing adapter describe -- <adapter command> [args]
//	bearing adapter sync --config cfg.json -- <adapter command> [args]
//
// and reads the graph store directly to answer questions about what Bearing
// knows, with the source, event, confidence and observed time of each fact:
//
//	bearing get <subject>
//	bearing owner <repo>
//	bearing related <subject>
//	bearing changes [--since <time>] [<subject>]
//
// and checks the audit log against checkpoints kept outside the store:
//
//	bearing audit verify --checkpoints cp.ndjson --key audit-1=audit-1.pub
//	bearing audit checkpoint --out cp.ndjson --key-id audit-1 --key-env AUDIT_KEY
//
// Each takes --store (or $BEARING_STORE); the queries also take --as-of. Run
// one with -h for its flags. A subject is a subject ID or a key such as
// github:repo/acme/payments. For changes, --since and --as-of are the two
// ends of the window, and the default end is now. Without --since it looks
// at the last 24 hours, newest change first, 100 at a time; a page that is
// not the last ends with the --page-token for the next.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

const usage = `usage:
  bearing adapter describe -- <adapter command> [args]
  bearing adapter sync [--config cfg.json] [--max-pages N] -- <adapter command> [args]
  bearing get <subject> [--store URL] [--as-of T] [--recorded-at T] [--json]
  bearing owner <repo> [--store URL] [--as-of T] [--recorded-at T] [--json]
  bearing related <subject> [--predicate P] [--store URL] [--as-of T] [--recorded-at T] [--json]
  bearing changes [--since T] [<subject>] [--axis valid|record] [--limit N] [--store URL] [--as-of T] [--json]
  bearing changes --page-token TOKEN [--limit N] [--store URL] [--json]
  bearing audit verify [--store URL] [--checkpoints FILE] [--key ID=FILE]... [--allow-unsigned] [--max-age D] [--json]
  bearing audit checkpoint [--store URL] [--out FILE] [--key-id ID --key-env VAR]
`

// version is the CLI version, set at build time with -ldflags.
var version = "dev"

func main() {
	os.Exit(mainCode())
}

func mainCode() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, telemetry.Config{ServiceName: "bearing-cli", ServiceVersion: version})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Telemetry is what failed to flush, so there is nowhere left to report it.
		_ = shutdown(ctx)
	}()
	if err != nil {
		// Telemetry isn't available, so stderr is the only place to say why.
		fmt.Fprintln(os.Stderr, "bearing: telemetry setup failed:", err)
		return 1
	}

	// One span per invocation, named after the subcommand, e.g. "bearing adapter sync".
	ctx, span := telemetry.Tracer("cmd/bearing").Start(ctx, spanName(os.Args[1:]))
	defer span.End()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		telemetry.Fail(ctx, span, telemetry.Logger("cmd/bearing"), "command failed", err)
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		return 1
	}
	return 0
}

var errUsage = errors.New("see usage below")

// spanName names the span of an invocation: the command and, for adapter,
// its subcommand. Arguments such as a subject never go in it.
func spanName(args []string) string {
	switch {
	case len(args) >= 2 && (args[0] == "adapter" || args[0] == "audit"):
		return "bearing " + args[0] + " " + args[1]
	case len(args) >= 1 && isQueryCommand(args[0]):
		return "bearing " + args[0]
	}
	return "bearing"
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	return runWith(ctx, defaultQueryEnv(), args, stdout)
}

func runWith(ctx context.Context, env queryEnv, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	if isQueryCommand(args[0]) {
		return queryCmd(ctx, env, args[0], args[1:], stdout)
	}
	switch args[0] {
	case "audit":
		return auditCmd(ctx, env, args[1:], stdout)
	case "adapter":
		if len(args) < 2 {
			return errUsage
		}
		switch args[1] {
		case "describe":
			return describe(ctx, args[2:], stdout)
		case "sync":
			return syncCmd(ctx, args[2:], stdout)
		}
	}
	return errUsage
}

func splitCommand(args []string) ([]string, []string, error) {
	for i, a := range args {
		if a == "--" {
			if i == len(args)-1 {
				return nil, nil, errors.New("missing adapter command after --")
			}
			return args[:i], args[i+1:], nil
		}
	}
	return nil, nil, errors.New("put the adapter command after --")
}

func describe(ctx context.Context, args []string, stdout io.Writer) (err error) {
	_, cmd, err := splitCommand(args)
	if err != nil {
		return err
	}
	c, err := adapter.Start(ctx, cmd[0], cmd[1:]...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	d, err := c.Describe(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

func syncCmd(ctx context.Context, args []string, stdout io.Writer) (err error) {
	flagArgs, cmd, err := splitCommand(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	configPath := fs.String("config", "", "adapter config JSON file")
	maxPages := fs.Int("max-pages", 10000, "stop if the adapter has not finished after this many pages")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	config := json.RawMessage(`{}`)
	if *configPath != "" {
		b, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		config = b
	}
	c, err := adapter.Start(ctx, cmd[0], cmd[1:]...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	w := bufio.NewWriter(stdout)
	defer func() { err = errors.Join(err, w.Flush()) }()
	n := 0
	sum, err := adapter.SyncAll(ctx, c, config, *maxPages, func(o *eventv1alpha1.Observation) error {
		n++
		b, err := model.EncodeJSON(o)
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	})
	telemetry.Logger("cmd/bearing").InfoContext(ctx, "observations written", "count", n)
	if err == nil && sum.RejectedObservations+sum.RejectedClaims > 0 {
		// The valid observations are written; the exit status says the adapter
		// is not clean, and the log names each rejection.
		err = fmt.Errorf("rejected %d observation(s) and %d claim(s); see the log", sum.RejectedObservations, sum.RejectedClaims)
	}
	return err
}
