// Command host is the WASM adapter spike's harness (see
// docs/spikes/wasm-adapters.md). It loads adapter modules on an Extism host
// with a wazero compilation cache, serves the single bearing_call host
// function (http, clock, log) against a GitHub fixture server, runs full
// syncs, compares the observations with the stdio adapter's, and prints the
// measurements as Markdown tables.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"

	extism "github.com/extism/go-sdk"

	"bearing.example/adapters/github"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

var (
	outDir   = flag.String("out", "../out", "directory with the built .wasm modules; binaries are built here too")
	rootDir  = flag.String("root", "../../..", "repository root, for building the stdio adapter and bearing")
	runs     = flag.Int("runs", 7, "repetitions for timing medians")
	fixtures = flag.String("fixtures", "testdata/github-fixtures.json.gz", "recorded GitHub API fixtures")
	record   = flag.Bool("record", false, "regenerate the fixtures file and exit")
	limits   = flag.Bool("limits", false, "bench each module twice: without, then with a 128 MiB memory cap, CloseOnContextDone and a 30 s per-sync deadline")
	mode     = flag.String("limit-mode", "both", "with -limits: both, mem (memory cap only) or close (CloseOnContextDone and deadline only)")
	only     = flag.String("only", "", "comma-separated globs of module file names to bench; skips the AWS and binary-size sections")
	embed    = flag.String("embed", "github-lean.tinygo.opt.wasm,github.go.wasm", "modules to embed for the binary size check")
)

// guestNow is what the clock capability reports, so WASM runs are repeatable.
var guestNow = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func main() {
	flag.Parse()
	limitMode = *mode
	if *record {
		if err := generateFixtures().save(*fixtures); err != nil {
			fatal(err)
		}
		return
	}
	ctx := context.Background()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	fs, err := loadFixtures(*fixtures)
	if err != nil {
		fatal(err)
	}
	var requests atomic.Int64
	srv := fs.serve(&requests)
	defer srv.Close()
	os.Setenv("SPIKE_GITHUB_TOKEN", fixtureToken)
	cfg := json.RawMessage(fmt.Sprintf(`{"org":%q,"api_url":%q}`, genOrg, srv.URL))

	var report bytes.Buffer
	w := io.MultiWriter(os.Stdout, &report)
	fmt.Fprintf(w, "Go %s, %d runs per timing (median), %d fixtures\n\n", strings.TrimPrefix(goVersion(), "go version "), *runs, len(fs))

	// Reference: the stdio adapter process, and the same code in-process.
	bin := filepath.Join(*outDir, "bin", "bearing-adapter-github")
	must(goBuild(*rootDir, bin, "./cmd/bearing-adapter-github"))
	var ref []model.Observation
	var stdioTimes []time.Duration
	var stdioRSS int64
	for i := 0; i < *runs; i++ {
		requests.Store(0)
		obs, d, rss, err := syncStdio(ctx, bin, cfg)
		must(err)
		stdioTimes = append(stdioTimes, d)
		stdioRSS = max(stdioRSS, rss)
		ref = obs
	}
	perSync := requests.Load()
	var nativeTimes []time.Duration
	for i := 0; i < *runs; i++ {
		a := &github.Adapter{HTTP: &http.Client{}, Now: time.Now, Getenv: func(string) string { return fixtureToken }}
		start := time.Now()
		must(adapter.SyncAll(ctx, a, cfg, 100, func(model.Observation) error { return nil }))
		nativeTimes = append(nativeTimes, time.Since(start))
	}

	fmt.Fprintf(w, "Reference sync: %d observations from %d GitHub API requests.\n\n", len(ref), perSync)
	fmt.Fprintln(w, "| Runtime | Module | Size | Cold compile | Warm compile (disk cache) | Instantiate + first call | Full sync (instance + sync) | Guest memory peak | bearing_call (first sync) | Output matches stdio |")
	fmt.Fprintln(w, "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- |")
	fmt.Fprintf(w, "| stdio process | `bearing-adapter-github` | %s | | | | %s | %s RSS | | reference |\n",
		mib(fileSize(bin)), ms(median(stdioTimes)), mib(stdioRSS*1024))
	fmt.Fprintf(w, "| native in-process | (linked in) | | | | | %s | | | |\n", ms(median(nativeTimes)))

	mods, _ := filepath.Glob(filepath.Join(*outDir, "github*.wasm"))
	raws, _ := filepath.Glob(filepath.Join(*outDir, "raw-github*.wasm"))
	sort.Strings(mods)
	sort.Strings(raws)
	modes := []bool{false}
	if *limits {
		modes = []bool{false, true}
	}
	for _, m := range append(mods, raws...) {
		if *only != "" && !slices.ContainsFunc(strings.Split(*only, ","), func(g string) bool {
			ok, _ := filepath.Match(g, filepath.Base(m))
			return ok
		}) {
			continue
		}
		for _, limited := range modes {
			benchRow(ctx, w, m, cfg, srv, ref, limited)
		}
	}
	if *only != "" {
		must(os.WriteFile(filepath.Join(*outDir, "results-limits.md"), report.Bytes(), 0o644))
		return
	}

	fmt.Fprintln(w, "\nAWS SDK for Go v2, sts:GetCallerIdentity through bearing_call against a stub:")
	fmt.Fprintln(w, "\n| Module | Size | Anonymous (host would sign) | SigV4-signed in guest (dummy keys) |")
	fmt.Fprintln(w, "| --- | ---: | --- | --- |")
	awsMods, _ := filepath.Glob(filepath.Join(*outDir, "aws-sts*.wasm"))
	sort.Strings(awsMods)
	for _, m := range awsMods {
		anon, signed := benchAWS(ctx, m)
		fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n", filepath.Base(m), mib(fileSize(m)), anon, signed)
	}

	fmt.Fprintln(w, "\nBinary size, `go build` defaults (as `make build`), linux/amd64:")
	fmt.Fprintln(w, "\n| Binary | Size | Stripped (-s -w) |")
	fmt.Fprintln(w, "| --- | ---: | ---: |")
	for _, b := range binarySizes(*embed) {
		fmt.Fprintf(w, "| %s | %s | %s |\n", b.name, mib(b.size), mib(b.stripped))
	}
	must(os.WriteFile(filepath.Join(*outDir, "results.md"), report.Bytes(), 0o644))
}

type moduleResult struct {
	cold, warm, first time.Duration
	syncs             []time.Duration
	memPeak           uint64
	calls             int64
	hostTime, capTime time.Duration
	bytesIn, bytesOut int64
	obs               []model.Observation
	denied            bool
	limitChecks       string
}

func containsDenied(err error) bool {
	return err != nil && strings.Contains(err.Error(), "permission denied")
}

func newHost(allowed string) *host {
	return &host{
		grant:  grant{HTTPHosts: []string{allowed}, HTTPMethods: []string{http.MethodGet}, TokenEnv: "SPIKE_GITHUB_TOKEN", Clock: true, Log: true},
		client: &http.Client{Timeout: 30 * time.Second},
		now:    func() time.Time { return guestNow },
		getenv: os.Getenv,
		log:    slog.Default(),
	}
}

func instanceConfig() extism.PluginInstanceConfig {
	mc := wazero.NewModuleConfig().WithSysWalltime().WithSysNanotime()
	if v := os.Getenv("SPIKE_GUEST_GOGC"); v != "" {
		mc = mc.WithEnv("GOGC", v) // upstream Go guests only
	}
	return extism.PluginInstanceConfig{ModuleConfig: mc}
}

func benchModule(ctx context.Context, path string, cfg json.RawMessage, srv *httptest.Server) (*moduleResult, error) {
	module, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	h := newHost(strings.TrimPrefix(srv.URL, "http://"))
	r := &moduleResult{}

	// Cold: empty on-disk compilation cache, as on first start.
	dir, err := os.MkdirTemp("", "wazero-cache-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	cp, err := compile(ctx, module, cache, h)
	if err != nil {
		return nil, err
	}
	r.cold = time.Since(start)
	cp.Close(ctx)
	cache.Close(ctx)

	// Warm: a new cache over the same directory, as after a restart.
	cache, err = wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, err
	}
	defer cache.Close(ctx)
	start = time.Now()
	cp, err = compile(ctx, module, cache, h)
	if err != nil {
		return nil, err
	}
	r.warm = time.Since(start)
	defer cp.Close(ctx)

	// First instance, with memory tracking: instantiate, init, describe.
	var mt memTracker
	tctx := experimental.WithMemoryAllocator(ctx, &mt)
	start = time.Now()
	p, err := cp.Instance(tctx, instanceConfig())
	if err != nil {
		return nil, err
	}
	if _, err := call(tctx, p, "describe", nil); err != nil {
		return nil, err
	}
	r.first = time.Since(start)
	h.calls.Store(0)
	h.bytesIn.Store(0)
	h.bytesOut.Store(0)
	h.hostNanos.Store(0)
	h.capNanos.Store(0)
	r.obs, err = syncWASM(tctx, p, cfg)
	if err != nil {
		return nil, err
	}
	r.memPeak, _ = mt.peaks()
	r.calls, r.bytesIn, r.bytesOut = h.calls.Load(), h.bytesIn.Load(), h.bytesOut.Load()
	r.hostTime, r.capTime = time.Duration(h.hostNanos.Load()), time.Duration(h.capNanos.Load())
	p.Close(ctx)

	// Repeated syncs, each on a fresh instance from the compiled module, as
	// a worker would run them.
	for i := 0; i < *runs; i++ {
		start := time.Now()
		sctx, cancel := withDeadline(ctx)
		p, err := cp.Instance(sctx, instanceConfig())
		if err != nil {
			cancel()
			return nil, err
		}
		_, err = syncWASM(sctx, p, cfg)
		cancel()
		if err != nil {
			return nil, err
		}
		r.syncs = append(r.syncs, time.Since(start))
		p.Close(ctx)
	}

	// The allowlist: a source pointing at an ungranted host must fail.
	p, err = cp.Instance(ctx, instanceConfig())
	if err != nil {
		return nil, err
	}
	_, err = syncWASM(ctx, p, json.RawMessage(`{"org":"acme","api_url":"https://api.evil.example"}`))
	r.denied = containsDenied(err)
	p.Close(ctx)

	if limitPages > 0 {
		r.limitChecks = checkLimits(func(pages uint32, deadline time.Duration) error {
			ctx := context.Background()
			c := cache
			if pages != limitPages {
				c = wazero.NewCompilationCache()
				defer c.Close(ctx)
			}
			cp, err := compileLimited(ctx, module, c, h, pages)
			if err != nil {
				return err
			}
			defer cp.Close(ctx)
			sctx, cancel := context.WithTimeout(ctx, deadline)
			defer cancel()
			p, err := cp.Instance(sctx, instanceConfig())
			if err != nil {
				return err
			}
			defer p.Close(ctx)
			_, err = syncWASM(sctx, p, cfg)
			return err
		})
	}
	return r, nil
}

// checkLimits shows the limits bite: a sync (instance + pages, after
// compile) under a 50 ms deadline must be interrupted, and one under a
// 10 MiB memory cap (the lean modules peak at 14 MiB or more) must fail
// rather than grow.
func checkLimits(sync func(pages uint32, deadline time.Duration) error) string {
	start := time.Now()
	err := sync(limitPages, 50*time.Millisecond)
	deadline := "50 ms deadline: NOT enforced"
	if err != nil {
		deadline = fmt.Sprintf("50 ms deadline: stopped; call returned after %s including warm compile (%s)", ms(time.Since(start)), oneLine(err))
	}
	err = sync(160, time.Minute)
	mem := "10 MiB cap: NOT enforced"
	if err != nil {
		mem = "10 MiB cap: failed as expected (" + oneLine(err) + ")"
	}
	return deadline + "; " + mem
}

// syncWASM pages through a full sync and validates every observation on the
// host, the way adapter.SyncAll does for stdio adapters.
func syncWASM(ctx context.Context, p *extism.Plugin, cfg json.RawMessage) ([]model.Observation, error) {
	var all []model.Observation
	cursor := ""
	for page := 1; page <= 100; page++ {
		in, _ := json.Marshal(adapter.SyncParams{Config: cfg, Cursor: cursor})
		out, err := call(ctx, p, "sync", in)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		var res adapter.SyncResult
		if err := json.Unmarshal(out, &res); err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		for _, o := range res.Observations {
			if err := o.Validate(); err != nil {
				return nil, fmt.Errorf("page %d: %w", page, err)
			}
		}
		all = append(all, res.Observations...)
		if res.Done {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return nil, adapter.ErrTooManyPages
}

// syncStdio runs the stdio adapter binary for one full sync and returns its
// observations, the wall time including process start, and its max RSS in KiB.
func syncStdio(ctx context.Context, bin string, cfg json.RawMessage) ([]model.Observation, time.Duration, int64, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "GITHUB_TOKEN="+fixtureToken)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, 0, 0, err
	}
	c := adapter.NewClient("github", stdout, stdin)
	var obs []model.Observation
	err := adapter.SyncAll(ctx, c, cfg, 100, func(o model.Observation) error { obs = append(obs, o); return nil })
	stdin.Close()
	if werr := cmd.Wait(); err == nil {
		err = werr
	}
	d := time.Since(start)
	var rss int64
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		rss = ru.Maxrss
	}
	return obs, d, rss, err
}

// compareObs compares observations ignoring time, which is the only thing
// that legitimately differs (the guest reads the host's fixed clock).
func compareObs(want, got []model.Observation) string {
	if len(want) != len(got) {
		return fmt.Sprintf("%d observations, want %d", len(got), len(want))
	}
	for i := range want {
		a, b := normalize(want[i]), normalize(got[i])
		if !reflect.DeepEqual(a, b) {
			return fmt.Sprintf("observation %d differs: %v vs %v", i, a["id"], b["id"])
		}
	}
	return ""
}

func normalize(o model.Observation) map[string]any {
	b, _ := json.Marshal(o)
	var m map[string]any
	json.Unmarshal(b, &m)
	delete(m, "time")
	if id, ok := m["id"].(string); ok {
		m["id"], _, _ = strings.Cut(id, "@")
	}
	return m
}

const stsResponse = `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <GetCallerIdentityResult>
    <Arn>arn:aws:iam::123456789012:user/spike</Arn>
    <UserId>AIDASPIKEEXAMPLE</UserId>
    <Account>123456789012</Account>
  </GetCallerIdentityResult>
  <ResponseMetadata><RequestId>01234567-89ab-cdef-0123-456789abcdef</RequestId></ResponseMetadata>
</GetCallerIdentityResponse>`

func benchAWS(ctx context.Context, path string) (anon, signed string) {
	var auth atomic.Value
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth.Store(r.Header.Get("Authorization"))
		if r.Method != http.MethodPost || !bytes.Contains(body, []byte("Action=GetCallerIdentity")) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, stsResponse)
	}))
	defer stub.Close()
	module, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), ""
	}
	h := newHost(strings.TrimPrefix(stub.URL, "http://"))
	h.grant.HTTPMethods = []string{http.MethodPost}
	h.grant.TokenEnv = "" // SigV4 would be the host's job; not implemented in the spike
	cache := wazero.NewCompilationCache()
	defer cache.Close(ctx)
	cp, err := compile(ctx, module, cache, h)
	if err != nil {
		return "compile failed: " + oneLine(err), ""
	}
	defer cp.Close(ctx)
	run := func(sign bool) string {
		p, err := cp.Instance(ctx, instanceConfig())
		if err != nil {
			return oneLine(err)
		}
		defer p.Close(ctx)
		auth.Store("")
		start := time.Now()
		out, err := call(ctx, p, "get_caller_identity", []byte(fmt.Sprintf(`{"endpoint":%q,"sign":%t}`, stub.URL, sign)))
		if err != nil {
			return "failed: " + oneLine(err)
		}
		a, _ := auth.Load().(string)
		scheme, _, _ := strings.Cut(a, " ")
		if scheme == "" {
			scheme = "none"
		}
		return fmt.Sprintf("ok in %s: %s; Authorization: %s", ms(time.Since(start)), out, scheme)
	}
	return run(false), run(true)
}

type binSize struct {
	name           string
	size, stripped int64
}

// binarySizes builds cmd/bearing as is, then a copy of it that links the
// Extism host (bearingplus tag), then that copy with a module embedded.
func binarySizes(embedList string) []binSize {
	binDir, err := filepath.Abs(filepath.Join(*outDir, "bin"))
	must(err)
	var out []binSize
	build := func(name, dir, pkg string, tags ...string) {
		b := binSize{name: name}
		for i, ld := range []string{"", "-s -w"} {
			o := filepath.Join(binDir, strings.NewReplacer(" ", "-", "`", "", "+", "plus", "(", "", ")", "").Replace(name)+[]string{"", ".stripped"}[i])
			args := []string{"build", "-trimpath", "-o", o}
			if ld != "" {
				args = append(args, "-ldflags="+ld)
			}
			if len(tags) > 0 {
				args = append(args, "-tags", strings.Join(tags, " "))
			}
			cmd := exec.Command("go", append(args, pkg)...)
			cmd.Dir = dir
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				b.name += " (build failed)"
				out = append(out, b)
				return
			}
			*[]*int64{&b.size, &b.stripped}[i] = fileSize(o)
		}
		out = append(out, b)
	}
	build("`bearing` (today)", *rootDir, "./cmd/bearing")

	src, err := os.ReadFile(filepath.Join(*rootDir, "cmd", "bearing", "main.go"))
	must(err)
	must(os.WriteFile(filepath.Join("bearingplus", "main.go"), src, 0o644))
	build("`bearing` + Extism/wazero host", ".", "./bearingplus", "bearingplus")
	for _, e := range strings.Split(embedList, ",") {
		mod, err := os.ReadFile(filepath.Join(*outDir, e))
		if err != nil {
			continue
		}
		must(os.WriteFile(filepath.Join("bearingplus", "adapter.wasm"), mod, 0o644))
		build(fmt.Sprintf("`bearing` + host + embedded `%s` (%s)", e, mib(int64(len(mod)))), ".", "./bearingplus", "bearingplus", "bearingembed")
	}
	return out
}

func goBuild(dir, out, pkg string) error {
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", abs, pkg)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func goVersion() string {
	out, _ := exec.Command("go", "version").Output()
	return strings.TrimSpace(string(out))
}

func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

func mib(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%.0f KiB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func oneLine(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return strings.ReplaceAll(s, "|", "/")
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "host:", err)
	os.Exit(1)
}

// benchRow benches one module and prints its table row. With limited, the
// module runs under C-ADAPTER-3-style limits (see runtimeConfig) and the
// row reports whether a tiny deadline and a small memory cap were enforced.
func benchRow(ctx context.Context, w io.Writer, m string, cfg json.RawMessage, srv *httptest.Server, ref []model.Observation, limited bool) {
	limitPages, syncDeadline = 0, 0
	if limited {
		limitPages, syncDeadline = 2048, 30*time.Second // 2048 pages = 128 MiB
	}
	kind := "Extism"
	if limited {
		kind = "Extism + limits"
	}
	var r *moduleResult
	var err error
	if strings.HasPrefix(filepath.Base(m), "raw-") {
		kind = strings.Replace(kind, "Extism", "raw wazero", 1)
		var module []byte
		if module, err = os.ReadFile(m); err == nil {
			r, err = benchRaw(ctx, module, cfg, newHost(strings.TrimPrefix(srv.URL, "http://")))
		}
	} else {
		r, err = benchModule(ctx, m, cfg, srv)
	}
	if err != nil {
		fmt.Fprintf(w, "| %s | `%s` | %s | failed: %v | | | | | | |\n", kind, filepath.Base(m), mib(fileSize(m)), oneLine(err))
		return
	}
	match := "yes"
	if diff := compareObs(ref, r.obs); diff != "" {
		match = "NO: " + diff
	}
	if !r.denied {
		match += "; allowlist NOT enforced"
	}
	fmt.Fprintf(w, "| %s | `%s` | %s | %s | %s | %s | %s | %s | %d calls, %s in / %s out; %s in host fn, of which %s in capabilities | %s |\n",
		kind, filepath.Base(m), mib(fileSize(m)), ms(r.cold), ms(r.warm), ms(r.first), ms(median(r.syncs)),
		mib(int64(r.memPeak)), r.calls, mib(r.bytesIn), mib(r.bytesOut), ms(r.hostTime), ms(r.capTime), match)
	if limited {
		fmt.Fprintf(w, "| %s | `%s` | checks: %s |\n", kind, filepath.Base(m), r.limitChecks)
	}
}
