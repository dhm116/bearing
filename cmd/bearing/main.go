// Command bearing is the Bearing developer CLI. It helps adapter authors run
// and check adapters, started from a command on a private socket or reached
// at the address of a running one over mutual TLS (the adapter's CA, and a
// client certificate and key the adapter trusts):
//
//	bearing adapter describe -- <adapter command> [args]
//	bearing adapter sync --config cfg.json -- <adapter command> [args]
//	bearing adapter sync --address https://host:port --tls-ca FILE --tls-cert FILE --tls-key FILE
//
// runs the server (bearing server --settings FILE),
// and answers questions about what Bearing knows, with the sources,
// confidence and validity of each fact, from the store directly or from a
// running server:
//
//	bearing get <ref>
//	bearing owner <repo>
//	bearing related <ref>
//	bearing changes [--since <time>] [<ref>]
//	bearing find <name>
//
// serves the read-only queries to an agent as an MCP server on stdin and
// stdout (see docs/mcp.md):
//
//	bearing mcp --server /run/bearing/api.sock
//
// and asks a running server to sync a source now:
//
//	bearing source sync github-acme --server /run/bearing/api.sock
//
// lists the identity links that wait for review, and records a person's
// decision on one through a running server (never a store, since decisions go
// through the server's event log):
//
//	bearing links --proposed
//	bearing link accept <link ID> --reason "same service" --server /run/bearing/api.sock
//	bearing link reject <link ID>
//	bearing link join <ref> <ref>
//	bearing link split <ref> <ref>
//	bearing link undo-distinct <link ID>
//
// The queries take --store (or $BEARING_STORE) to read a store, or --server (or
// $BEARING_SERVER) to ask a running server, which answers the same questions
// the same way: its Unix socket, which needs no token, or https://host:port with
// a bearer token in the variable --token-env names ($BEARING_TOKEN). They also
// take --as-of. Run one with -h for its flags. A ref is an entity ID or a key
// such as github:repo/acme/payments. For changes, --since and --as-of are the
// two ends of the window, and the default end is now. Without --since it looks
// at the last 24 hours, newest change first.
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
	"path/filepath"
	"slices"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	_ "bearing.example/internal/duckstore" // registers the mem and duckdb store schemes
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

const usage = `usage:
  bearing adapter describe [--address URL --tls-ca FILE --tls-cert FILE --tls-key FILE [--tls-server-name NAME | --tls-peer-id SPIFFE_ID] | -- <adapter command> [args]]
  bearing adapter sync [--config cfg.json] [--max-pages N] [--address URL --tls-ca FILE --tls-cert FILE --tls-key FILE [--tls-server-name NAME | --tls-peer-id SPIFFE_ID] | -- <adapter command> [args]]
  bearing get <ref> [--store URL | --server ADDR] [--as-of T] [--json]
  bearing owner <repo> [--store URL | --server ADDR] [--as-of T] [--json]
  bearing related <ref> [--predicate P] [--direction out|in] [--depth N] [--store URL | --server ADDR] [--as-of T] [--json]
  bearing changes [--since T] [<ref>] [--limit N] [--store URL | --server ADDR] [--as-of T] [--json]
  bearing find <name> [--kind K] [--limit N] [--store URL | --server ADDR] [--json]
  bearing links [--proposed] [--entity REF] [--limit N] [--store URL | --server ADDR] [--json]
  bearing link accept|reject|undo-distinct <link ID> [--reason TEXT] --server ADDR [--json]
  bearing link join|split <ref> <ref> [--reason TEXT] --server ADDR [--json]
  bearing mcp [--store URL | --server ADDR]
  bearing server --settings FILE
  bearing source sync <source> [--server ADDR] [--json]
  (--server also takes --token-env VAR and --server-ca FILE; $BEARING_SERVER and $BEARING_TOKEN stand in for the flags)
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
	case len(args) >= 2 && (args[0] == "adapter" || args[0] == "source" || args[0] == "link"):
		return "bearing " + args[0] + " " + args[1]
	case len(args) >= 1 && (isQueryCommand(args[0]) || args[0] == "server" || args[0] == "mcp"):
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
	case "server":
		return serverCmd(ctx, args[1:], stdout)
	case "source":
		return sourceCmd(ctx, env, args[1:], stdout)
	case "mcp":
		return mcpCmd(ctx, env, args[1:], stdout)
	case "link":
		return linkCmd(ctx, env, args[1:], stdout)
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

// openAdapter reaches the adapter args name: a command after --, started on a
// socket in a temporary directory, or the running adapter at --address. fs
// holds the caller's own flags; done stops whatever was started.
func openAdapter(ctx context.Context, fs *flag.FlagSet, args []string) (a adapter.Adapter, done func() error, err error) {
	flagArgs, cmd := args, []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		flagArgs, cmd = args[:i], args[i+1:]
		if len(cmd) == 0 {
			return nil, nil, fmt.Errorf("missing adapter command after --: %w", errUsage)
		}
	}
	fs.SetOutput(io.Discard)
	address := fs.String("address", "", "reach the running adapter at https://host:port instead of starting a command")
	var dc adapter.DialConfig
	fs.StringVar(&dc.CAFile, "tls-ca", "", "for --address: the CA that signed the adapter's certificate (the system roots are not trusted)")
	fs.StringVar(&dc.CertFile, "tls-cert", "", "for --address: the client certificate to present")
	fs.StringVar(&dc.KeyFile, "tls-key", "", "for --address: the client certificate's key, readable by its owner only")
	fs.StringVar(&dc.ServerName, "tls-server-name", "", "for --address: the name the adapter's certificate must hold (default the address's host)")
	fs.StringVar(&dc.PeerID, "tls-peer-id", "", "for --address: the SPIFFE ID the adapter's certificate must hold, checked in place of a host name")
	if err := fs.Parse(flagArgs); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", err, errUsage)
	}
	switch {
	case fs.NArg() > 0:
		return nil, nil, fmt.Errorf("put the adapter command after --: %w", errUsage)
	case *address != "" && cmd != nil:
		return nil, nil, fmt.Errorf("give --address or an adapter command after --, not both: %w", errUsage)
	case *address != "":
		if dc.CAFile == "" || dc.CertFile == "" || dc.KeyFile == "" {
			return nil, nil, errors.New("--address needs --tls-ca, --tls-cert and --tls-key: a remote adapter is reached over mutual TLS")
		}
		dc.Address = *address
		r, err := adapter.Dial("remote", dc)
		if err != nil {
			return nil, nil, err
		}
		return r, func() error { return nil }, nil
	case cmd == nil:
		return nil, nil, fmt.Errorf("put the adapter command after --, or give --address: %w", errUsage)
	}
	dir, err := os.MkdirTemp("", "bearing-")
	if err != nil {
		return nil, nil, err
	}
	p, err := adapter.Start(ctx, adapter.StartConfig{
		Name: filepath.Base(cmd[0]), Command: cmd[0], Args: cmd[1:], InheritEnv: true, SocketDir: dir,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	return p, func() error {
		err := p.Close()
		return errors.Join(err, os.RemoveAll(dir))
	}, nil
}

func describe(ctx context.Context, args []string, stdout io.Writer) (err error) {
	a, done, err := openAdapter(ctx, flag.NewFlagSet("describe", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, done()) }()
	d, err := a.Describe(ctx)
	if err != nil {
		return err
	}
	return writeJSON(stdout, d)
}

func syncCmd(ctx context.Context, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	configPath := fs.String("config", "", "adapter config JSON file")
	maxPages := fs.Int("max-pages", 10000, "stop if the adapter has not finished after this many pages")
	a, done, err := openAdapter(ctx, fs, args)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, done()) }()
	config := json.RawMessage(`{}`)
	if *configPath != "" {
		b, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		config = b
	}
	w := bufio.NewWriter(stdout)
	defer func() { err = errors.Join(err, w.Flush()) }()
	n := 0
	sum, err := adapter.SyncAll(ctx, a, config, *maxPages, func(o *eventv1alpha1.Observation) error {
		n++
		b, err := model.EncodeJSON(o)
		if err != nil {
			return err
		}
		_, err = w.Write(append(jsonSafe(b), '\n'))
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
