// Command bearing-bench measures a Bearing store at the design target of
// ADR 14: thousands of repositories and people and about 10 million facts.
//
//	bearing-bench load    --store URL --facts N --out results.ndjson
//	bearing-bench measure --store URL --out results.ndjson
//	bearing-bench merges  --store URL --out results.ndjson
//	bearing-bench history --store URL --out results.ndjson
//
// load feeds a generated organization to the store through the resolver, as
// the sources would, and measures the store at each checkpoint; measure
// measures a store that is already loaded; merges and history are the
// experiments that need a store of their own. Results are written as JSON
// lines. See docs/benchmarks/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"bearing.example/pkg/resolver"
	"bearing.example/pkg/store"
)

const usage = `usage:
  bearing-bench load    --store URL [--facts N] [--checkpoints N,N,...] [--out FILE] [org flags]
  bearing-bench measure --store URL [--label NAME] [--out FILE] [org flags]
  bearing-bench merges  --store URL [--out FILE]
  bearing-bench history --store URL [--out FILE]

The store URL is a postgres:// one (password in BEARING_STORE_PASSWORD) or
mem://. Run each experiment against a schema of its own (?schema=name).
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "bearing-bench:", err)
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		os.Exit(1)
	}
}

var errUsage = errors.New("see usage below")

// flags are the options every subcommand shares.
type flags struct {
	store        string
	out          string
	declarations string
	label        string
	facts        int64
	events       int64
	checkpoints  string
	bulkAfter    int
	quick        bool
	only         string
	flip         bool
	org          orgConfig
}

func (f *flags) register(fs *flag.FlagSet) {
	d := defaultOrg()
	fs.StringVar(&f.store, "store", os.Getenv("BEARING_STORE"), "store URL")
	fs.StringVar(&f.out, "out", "", "file to append results to (default: standard output is not used; results are dropped)")
	fs.StringVar(&f.declarations, "declarations", "testdata/declarations", "directory of the reference adapter declarations")
	fs.StringVar(&f.label, "label", "", "name for this measurement")
	fs.Int64Var(&f.facts, "facts", 10_000_000, "fact rows to load")
	fs.Int64Var(&f.events, "events", 0, "stop after this many events of the stream (the only limit for a store that is not PostgreSQL)")
	fs.StringVar(&f.checkpoints, "checkpoints", "100000,300000,1000000,3000000,10000000", "fact-row counts at which to measure")
	fs.BoolVar(&f.quick, "quick", false, "run each measurement for a fraction of a second, to check the benchmark itself")
	fs.StringVar(&f.only, "only", "", `measure: "resolve" runs only the resolver timings`)
	fs.BoolVar(&f.flip, "flip", false, "history: change one repository at every read")
	fs.IntVar(&f.bulkAfter, "bulk-after-day", 5, "simulated day from which Change events are applied without resolving them")
	fs.Uint64Var(&f.org.Seed, "seed", d.Seed, "seed of the generated org")
	fs.IntVar(&f.org.Repos, "repos", d.Repos, "repositories at the first sync")
	fs.IntVar(&f.org.People, "people", d.People, "people at the first sync")
	fs.IntVar(&f.org.Teams, "teams", d.Teams, "teams at the first sync")
	fs.IntVar(&f.org.LinkedPercent, "linked-percent", d.LinkedPercent, "share of people with a directory account (each is a merge)")
	fs.IntVar(&f.org.ChangesPerDay, "changes-per-day", d.ChangesPerDay, "merged pull requests a day")
	fs.IntVar(&f.org.RepoEditsPerDay, "repo-edits-per-day", d.RepoEditsPerDay, "repository edits a day")
	fs.IntVar(&f.org.OwnerEditsPerDay, "owner-edits-per-day", d.OwnerEditsPerDay, "CODEOWNERS changes a day")
	fs.IntVar(&f.org.MemberEditsPerDay, "member-edits-per-day", d.MemberEditsPerDay, "team membership changes a day")
	fs.IntVar(&f.org.RenamesPerDay, "renames-per-day", d.RenamesPerDay, "repository renames a day")
	fs.IntVar(&f.org.NewReposPerDay, "new-repos-per-day", d.NewReposPerDay, "repositories created a day")
	fs.IntVar(&f.org.ResyncEveryDays, "resync-every-days", d.ResyncEveryDays, "days between full reads of the org")
	f.org.Start = d.Start
}

func parseCounts(s string) ([]int64, error) {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("bad count %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func run(ctx context.Context, args []string, log io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f flags
	f.register(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 || f.store == "" {
		return fmt.Errorf("%w: a --store URL and no other arguments are required", errUsage)
	}
	out := io.Discard
	if f.out != "" {
		file, err := os.OpenFile(f.out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G302/G304: the operator names the results file
		if err != nil {
			return err
		}
		defer file.Close() //nolint:errcheck // results were written line by line
		out = file
	}
	w, err := newWorld(ctx, f, out, log)
	if err != nil {
		return err
	}
	defer w.close()
	switch cmd {
	case "load":
		cps, err := parseCounts(f.checkpoints)
		if err != nil {
			return err
		}
		return w.runLoad(f, cps)
	case "measure":
		return w.runMeasure(f)
	case "merges":
		return w.runMerges(f)
	case "history":
		return w.runHistory(f)
	}
	return errUsage
}

func newWorld(ctx context.Context, f flags, out, log io.Writer) (*world, error) {
	cfg, err := resolverConfig(f.declarations)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, store.Config{Graph: f.store})
	if err != nil {
		return nil, err
	}
	p, err := openPG(ctx, f.store)
	if err != nil {
		_ = st.Close(ctx)
		return nil, err
	}
	res, err := resolver.New(cfg, st.Graph)
	if err != nil {
		_ = st.Close(ctx)
		return nil, err
	}
	tpl, err := newChangeTemplate(ctx, cfg)
	if err != nil {
		_ = st.Close(ctx)
		return nil, err
	}
	w := &world{
		ctx: ctx, st: st, graph: st.Graph, pg: p, res: res, rcfg: cfg, tpl: tpl, log: log, now: time.Now,
		out: &sink{w: out, now: time.Now}, stream: newStream(f.org), bulkAfter: f.bulkAfter, people: map[int]string{},
	}
	return w, nil
}

func (w *world) close() {
	w.pg.close(context.WithoutCancel(w.ctx))
	_ = w.st.Close(context.WithoutCancel(w.ctx))
}
