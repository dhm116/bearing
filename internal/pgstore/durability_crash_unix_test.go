//go:build unix

package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
)

// A step is a point in a transaction where a process can be stopped. Every
// transaction offers, in order: before it begins, before each statement,
// before its commit and after its commit has been acknowledged.
const (
	stepBegin     = 0  // before BEGIN
	stepCommit    = -1 // after the last statement, before COMMIT
	stepCommitted = -2 // after COMMIT returned
)

// txRecord is what a transcript keeps of one transaction.
type txRecord struct {
	N          int      // 1-based, in the order the transactions began
	Statements []string // the SQL, whitespace collapsed
}

// kind names what the transaction did, from the statements it ran.
func (r txRecord) kind() string {
	all := strings.Join(r.Statements, " ")
	switch {
	case strings.Contains(all, "INSERT INTO journal"):
		return "apply"
	case strings.Contains(all, "INSERT INTO event_offset"):
		return "offset"
	case strings.Contains(all, "INSERT INTO event"):
		return "append"
	}
	return "read"
}

// crashPool wraps the store's pool and calls hook at every step of every
// transaction. hook may stop the process (see killSelf) or record what it saw.
type crashPool struct {
	pool
	hook func(tx int, step int, sql string)

	mu sync.Mutex
	n  int
}

func (p *crashPool) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()
	p.hook(n, stepBegin, "")
	tx, err := p.pool.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &crashTx{Tx: tx, p: p, n: n}, nil
}

type crashTx struct {
	pgx.Tx
	p     *crashPool
	n     int
	steps int
}

func (t *crashTx) step(sql string) {
	t.steps++
	t.p.hook(t.n, t.steps, strings.Join(strings.Fields(sql), " "))
}

func (t *crashTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.step(sql)
	return t.Tx.Exec(ctx, sql, args...)
}

func (t *crashTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.step(sql)
	return t.Tx.Query(ctx, sql, args...)
}

func (t *crashTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.step(sql)
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t *crashTx) Commit(ctx context.Context) error {
	t.p.hook(t.n, stepCommit, "")
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	t.p.hook(t.n, stepCommitted, "")
	return nil
}

// killSelf ends the process the way an operator's kill -9 does: no deferred
// call runs, no connection is closed politely, nothing is flushed.
func killSelf() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	select {} // the signal is delivered before this blocks for long
}

// childEnv carries a childConfig to the test binary this file starts again.
const childEnv = "BEARING_DURABILITY_CHILD"

// childAuthEnv carries the database password, which stays out of the
// JSON.
const childAuthEnv = "BEARING_DURABILITY_CHILD_AUTH"

// killPoint is a step of a transaction of the child's run, counted from the
// first transaction the child opens.
type killPoint struct {
	Tx   int
	Step int
	// SQL is the start of the statement the child must be about to run at a
	// numbered step. The child stops with an error instead of dying when the
	// statement is another, because then the run it was meant to repeat
	// went differently and the test would prove nothing.
	SQL string
}

// childConfig tells a child process which database to use and where to die.
type childConfig struct {
	Host                    string
	Port                    uint16
	Database, User, SSLMode string
	Schema                  string
	AllowSuperuser          bool
	password                string
	// Fixed gives the store a clock that never moves and IDs that depend on
	// nothing else, so that a run which was stopped and started again can be
	// compared byte for byte with one that was not. Without it the store
	// uses the wall clock and random IDs, as a server does.
	Fixed bool
	// Kill is where the child kills itself; nil lets it finish.
	Kill *killPoint
	// Transcript is a file the child writes its transactions to when it
	// finishes: a reference run's list of what a run does.
	Transcript string
}

func newChildConfig(o Options) childConfig {
	return childConfig{
		Host: o.Host, Port: o.Port, Database: o.Database, User: o.User, password: o.Password,
		SSLMode: o.SSLMode, Schema: o.Schema, AllowSuperuser: o.AllowSuperuser,
	}
}

func (c childConfig) options() Options {
	return Options{
		Host: c.Host, Port: c.Port, Database: c.Database, User: c.User, Password: os.Getenv(childAuthEnv), SSLMode: c.SSLMode,
		Schema: c.Schema, AllowSuperuser: c.AllowSuperuser, MaxConns: 2, ApplicationName: "bearing-durability-child",
	}
}

// TestDurabilityChild is the process the tests above kill: the apply worker
// on a store whose pool reports every step of every transaction. Run alone
// it does nothing.
func TestDurabilityChild(t *testing.T) {
	raw := os.Getenv(childEnv)
	if raw == "" {
		t.Skip("started by the durability tests, not on its own")
	}
	var cfg childConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("%s: %v", childEnv, err)
	}
	ctx := context.Background()
	s, err := Open(ctx, cfg.options())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fixed {
		s.Now, s.IDs = fixedClock(), testkit.NewUUIDv7s(fixedClock())
	}
	var mu sync.Mutex
	txs := map[int]*txRecord{}
	s.db = &crashPool{pool: s.db, hook: func(tx, step int, sql string) {
		if k := cfg.Kill; k != nil && tx == k.Tx && step == k.Step {
			if !strings.HasPrefix(sql, k.SQL) {
				t.Errorf("transaction %d step %d is %q, want %q: the run is not the one that was meant to be repeated", tx, step, sql, k.SQL)
				os.Exit(3)
			}
			killSelf()
		}
		if cfg.Transcript == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		r := txs[tx]
		if r == nil {
			r = &txRecord{N: tx}
			txs[tx] = r
		}
		if step > 0 {
			r.Statements = append(r.Statements, sql)
		}
	}}
	w := &worker{log: s, store: s, resolver: storyResolver(t, s), group: storyGroup}
	if _, err := w.drain(ctx); err != nil {
		t.Fatal(err)
	}
	if cfg.Transcript != "" {
		list := make([]*txRecord, 0, len(txs))
		for _, n := range slices.Sorted(mapKeys(txs)) {
			list = append(list, txs[n])
		}
		b, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.Transcript, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// child is a run of the test binary as the worker.
type child struct {
	cmd    *exec.Cmd
	out    bytes.Buffer
	secret string // the database password, which the output must not hold
}

// tail is the end of the child's output, enough to see why it failed.
func (c *child) tail() string {
	const limit = 8 << 10
	s := c.out.String()
	if len(s) > limit {
		s = "..." + s[len(s)-limit:]
	}
	return s
}

// childEnviron is the environment a child gets: what a Go test binary needs
// to run, and the configuration, but none of the parent's credentials.
func childEnviron(config, password string) []string {
	env := []string{childEnv + "=" + config, childAuthEnv + "=" + password}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "GOCOVERDIR", "GOMAXPROCS"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func startChild(t testing.TB, cfg childConfig) *child {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := &child{secret: cfg.password}
	c.cmd = exec.Command(exe, "-test.run=^TestDurabilityChild$", "-test.count=1", "-test.timeout=5m") //nolint:gosec // G204: the test binary running itself
	c.cmd.Env = childEnviron(string(b), cfg.password)
	c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.out
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// A test that fails before it waits does not leave the worker running
	// against a schema that is about to be dropped. Killing a process that
	// has been waited for does nothing.
	t.Cleanup(func() { _ = c.cmd.Process.Kill() })
	return c
}

// wait waits for the child and says whether SIGKILL ended it. Any other
// failure of the child fails the test with its output.
func (c *child) wait(t testing.TB) (killed bool) {
	t.Helper()
	err := c.cmd.Wait()
	testkit.AssertNoLeaks(t, c.out.Bytes(), c.secret)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return false
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			return true
		}
	}
	t.Fatalf("child failed: %v\n%s", err, c.tail())
	return false
}

// runToEnd runs a child that is not killed, and fails the test unless it
// finishes.
func runToEnd(t testing.TB, cfg childConfig) {
	t.Helper()
	cfg.Kill = nil
	if c := startChild(t, cfg); c.wait(t) {
		t.Fatalf("child was killed\n%s", c.tail())
	}
}

// crashRun is a store on a schema of its own with the story on its log.
type crashRun struct {
	t      *testing.T
	store  *Store
	opts   Options
	events []contracts.Event
}

func newCrashRun(t *testing.T, events []contracts.Event) *crashRun {
	t.Helper()
	s, o := openTestStore(t)
	if _, err := s.Append(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	return &crashRun{t: t, store: s, opts: o, events: events}
}

func (r *crashRun) config() childConfig { return newChildConfig(r.opts) }

// fixed is config with the fixed clock and IDs.
func (r *crashRun) fixed() childConfig {
	c := r.config()
	c.Fixed = true
	return c
}

// transcript is what a reference run did.
func readTranscript(t testing.TB, path string) []txRecord {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file this test wrote
	if err != nil {
		t.Fatal(err)
	}
	var out []txRecord
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// failpoint is one place in a run to kill the process, named for the
// transaction that holds it.
type failpoint struct {
	name string
	kill killPoint
	// applied is how many applies the store must hold afterwards, before the
	// restarted worker finishes: the ones before the transaction, and that
	// one too if the kill came after its commit.
	applied int
}

// failpoints lists the places to kill the worker, from the reference run's
// transcript. A step is a point in a transaction: before BEGIN, before each
// statement, before COMMIT is sent and after it is acknowledged. By default
// the worker is killed with COMMIT about to be sent and with it acknowledged
// in the first and last apply, before the first write of each kind that
// any apply runs (so every kind of write is cut off with the writes before it
// in the transaction, and a merge record, a closed version or a component
// join is not left out because it first appears late), and at every step of
// the first offset commit. With all it is every step of every apply as well.
// Only the client dies: the PostgreSQL server keeps running, so what its own
// crash does to the WAL is not tested here.
func failpoints(txs []txRecord, all bool) []failpoint {
	var applies []txRecord
	for _, tx := range txs {
		if tx.kind() == "apply" {
			applies = append(applies, tx)
		}
	}
	var out []failpoint
	seen := map[[2]int]bool{}
	add := func(tx txRecord, pos, step int) {
		if seen[[2]int{tx.N, step}] {
			return
		}
		seen[[2]int{tx.N, step}] = true
		sql := ""
		if step > 0 {
			sql = tx.Statements[step-1]
		}
		label := fmt.Sprint("step ", step)
		switch step {
		case stepBegin:
			label = "before begin"
		case stepCommit:
			label = "before commit"
		case stepCommitted:
			label = "after commit"
		}
		f := failpoint{
			name: fmt.Sprintf("%s %d %s", tx.kind(), pos, label),
			kill: killPoint{Tx: tx.N, Step: step, SQL: sql[:min(len(sql), 60)]},
		}
		for _, other := range applies {
			if other.N < tx.N {
				f.applied++
			}
		}
		if tx.kind() == "apply" && step == stepCommitted {
			f.applied++
		}
		out = append(out, f)
	}
	every := func(tx txRecord, pos int) {
		add(tx, pos, stepBegin)
		for i := range tx.Statements {
			add(tx, pos, i+1)
		}
		add(tx, pos, stepCommit)
		add(tx, pos, stepCommitted)
	}
	if all {
		for i, tx := range applies {
			every(tx, i+1)
		}
	} else {
		last := len(applies) - 1
		for _, i := range []int{0, last} {
			add(applies[i], i+1, stepCommit)
			add(applies[i], i+1, stepCommitted)
		}
		kinds := map[string]bool{}
		for i, tx := range applies {
			for j, s := range tx.Statements {
				if strings.HasPrefix(s, "SELECT") {
					continue // reads change nothing a kill could leave half done
				}
				k := strings.Join(strings.Fields(s), " ")
				k = k[:min(len(k), 30)]
				if !kinds[k] {
					kinds[k] = true
					add(tx, i+1, j+1)
				}
			}
		}
	}
	// The commit of the first offset, every step: the worker is between
	// applying an entry and telling the log.
	for _, tx := range txs {
		if tx.kind() == "offset" {
			every(tx, 1)
			break
		}
	}
	return out
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

func durabilityAll() bool { return os.Getenv("BEARING_TEST_DURABILITY") == "all" }

// The worker is killed with SIGKILL at each of the places listed by
// failpoints. Afterwards the store holds exactly the applies that had
// committed, with a chain that verifies, and the worker started again
// finishes the log into the graph an uninterrupted run gives.
func TestKillingTheWorkerAtAnyApplyFailpointLosesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := storyEvents(t)

	ref := newCrashRun(t, events)
	cfg := ref.config()
	cfg.Transcript = filepath.Join(t.TempDir(), "transcript.json")
	runToEnd(t, cfg)
	want := canonicalDump(ctx, t, ref.store, dumpCanonical)
	verifyAuditChain(ctx, t, ref.store)
	if want.AuditRecords == 0 {
		t.Fatal("the story wrote no audit records, so a verifying chain would prove nothing")
	}
	points := failpoints(readTranscript(t, cfg.Transcript), durabilityAll())
	t.Logf("%d failpoints; the uninterrupted run applied %d journal entries and wrote %d audit records", len(points), len(want.Journal), want.AuditRecords)

	for _, fp := range points {
		t.Run(fp.name, func(t *testing.T) {
			t.Parallel()
			run := newCrashRun(t, events)
			cfg := run.config()
			cfg.Kill = &fp.kill
			c := startChild(t, cfg)
			if !c.wait(t) {
				t.Fatalf("the worker finished instead of dying at %+v\n%s", fp.kill, c.tail())
			}

			// What a crash leaves: whole applies only.
			got := canonicalDump(ctx, t, run.store, dumpCanonical)
			if !slices.Equal(got.Journal, want.Journal[:fp.applied]) {
				t.Fatalf("after the kill the journal holds %d entries, want the first %d of the uninterrupted run's:\n%s",
					len(got.Journal), fp.applied, firstDifference(strings.Join(want.Journal[:min(len(got.Journal), fp.applied)], "\n"), strings.Join(got.Journal, "\n")))
			}
			verifyAuditChain(ctx, t, run.store)
			// Every change has its audit records and no other record exists.
			if wantRecords := sum(want.Audited[:fp.applied]); got.AuditRecords != wantRecords {
				t.Fatalf("after the kill the audit log holds %d records, want the %d of the %d applies that committed", got.AuditRecords, wantRecords, fp.applied)
			}

			// The worker comes back and finishes.
			runToEnd(t, run.config())
			final := canonicalDump(ctx, t, run.store, dumpCanonical)
			if final.String() != want.String() {
				t.Fatalf("after the restart the graph differs from an uninterrupted run:\n%s", firstDifference(want.String(), final.String()))
			}
			verifyAuditChain(ctx, t, run.store)
			assertLogConsumed(ctx, t, run.store)
		})
	}
}

// assertLogConsumed fails unless the worker's group has committed every
// partition up to its head.
func assertLogConsumed(ctx context.Context, t testing.TB, s *Store) {
	t.Helper()
	parts, err := s.Partitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		got, err := s.Committed(ctx, storyGroup, p.Partition)
		if err != nil {
			t.Fatal(err)
		}
		if got != p.Head {
			t.Errorf("partition %s: the worker committed offset %d, the head is %d", p.Partition, got, p.Head)
		}
	}
}

// Stopping the worker at random moments, many times in a row, still ends in
// the graph an uninterrupted run gives. The moments come from a fixed seed,
// but where they land in the worker's steps depends on timing, so a failure
// may not repeat; the failpoints above are the exact cases.
func TestKillingTheWorkerAtRandomMomentsAgainAndAgainLosesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := storyEvents(t)

	ref := newCrashRun(t, events)
	started := time.Now()
	runToEnd(t, ref.fixed())
	uninterrupted := time.Since(started)
	want := canonicalDump(ctx, t, ref.store, dumpExact)

	run := newCrashRun(t, events)
	rng := rand.New(rand.NewPCG(140, 140)) //nolint:gosec // G404: a seeded generator makes the schedule of kills repeatable
	kills, midway := 0, 0
	// Each run gets between a twentieth and a quarter of the time an
	// uninterrupted run took, but the worker resumes where the log left off,
	// so the runs add up.
	for range 6 {
		c := startChild(t, run.fixed())
		window := uninterrupted/20 + time.Duration(rng.Int64N(int64(uninterrupted/5)))
		timer := time.AfterFunc(window, func() { _ = c.cmd.Process.Kill() })
		killed := c.wait(t)
		timer.Stop()
		if !killed {
			break
		}
		kills++
		got := canonicalDump(ctx, t, run.store, dumpExact)
		if len(got.Journal) > len(want.Journal) || !slices.Equal(got.Journal, want.Journal[:len(got.Journal)]) {
			t.Fatalf("after kill %d the journal is not a prefix of the uninterrupted run's:\n%s", kills, firstDifference(want.String(), got.String()))
		}
		if n := len(got.Journal); n > 0 && n < len(want.Journal) {
			midway++
		}
		verifyAuditChain(ctx, t, run.store)
	}
	if midway < 2 {
		t.Fatalf("%d of %d killed runs stopped with the story partly applied, so the test did not exercise recovery", midway, kills)
	}
	t.Logf("%d runs were killed before one finished, %d of them with the story partly applied", kills, midway)

	runToEnd(t, run.fixed())
	final := canonicalDump(ctx, t, run.store, dumpExact)
	if final.String() != want.String() {
		t.Fatalf("after %d kills the graph differs from an uninterrupted run:\n%s", kills, firstDifference(want.String(), final.String()))
	}
	verifyAuditChain(ctx, t, run.store)
	assertLogConsumed(ctx, t, run.store)
}
