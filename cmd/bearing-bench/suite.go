package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// environment says what the numbers were taken on.
type environment struct {
	CPUModel     string            `json:"cpu_model"`
	CPUs         int               `json:"cpus"`
	MemoryMB     int               `json:"memory_mb"`
	Kernel       string            `json:"kernel"`
	GoVersion    string            `json:"go_version"`
	Commit       string            `json:"bench_commit,omitempty"`
	Store        string            `json:"store"`
	PostgreSQL   string            `json:"postgresql,omitempty"`
	PGSettings   map[string]string `json:"postgresql_settings,omitempty"`
	Org          orgConfig         `json:"org"`
	BulkAfterDay int               `json:"bulk_after_day"`
}

func (w *world) environment(f flags) environment {
	e := environment{CPUs: runtime.NumCPU(), GoVersion: runtime.Version(), Store: redactURL(f.store), Org: f.org, BulkAfterDay: f.bulkAfter}
	if fh, err := os.Open("/proc/cpuinfo"); err == nil {
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "model name"); ok {
				e.CPUModel = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), ":"))
				break
			}
		}
		_ = fh.Close()
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for line := range strings.SplitSeq(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
				kb, _ := strconv.Atoi(strings.Fields(v)[0])
				e.MemoryMB = kb / 1024
			}
		}
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		e.Kernel = strings.TrimSpace(string(b))
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				e.Commit = s.Value
			}
		}
	}
	if w.pg != nil {
		e.PostgreSQL, _ = w.pg.version(w.ctx)
		e.PGSettings, _ = w.pg.settings(w.ctx)
	}
	return e
}

// redactURL drops anything after the host from a store URL's user info.
func redactURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			user, _, _ := strings.Cut(rest[:at], ":")
			return u[:i+3] + user + "@" + rest[at+1:]
		}
	}
	return u
}

// suiteOptions say how long the measurements run.
type suiteOptions struct {
	// Duration is how long each read runs per caller count.
	Duration time.Duration
	// Applies is how long each group of writers runs.
	Applies time.Duration
	// Callers are the read concurrencies.
	Callers []int
	// Writers are the write concurrencies.
	Writers []int
}

func defaultSuite(quick bool) suiteOptions {
	if quick {
		return suiteOptions{Duration: 300 * time.Millisecond, Applies: time.Second, Callers: []int{1, 4}, Writers: []int{1, 4}}
	}
	return suiteOptions{Duration: 8 * time.Second, Applies: 20 * time.Second, Callers: []int{1, 16}, Writers: []int{1, 4, 16}}
}

// measure runs the whole suite on the store as it stands, filing the results
// under the number of fact rows it holds.
func (w *world) measure(label string, facts int64, o suiteOptions) error {
	if w.pg != nil {
		w.logf("measure %s: vacuum and analyze", label)
		if err := w.pg.analyze(w.ctx); err != nil {
			return err
		}
		rc, err := w.pg.rowCounts(w.ctx)
		if err != nil {
			return err
		}
		w.out.put("rows", label, facts, rc)
		sz, err := w.pg.sizes(w.ctx)
		if err != nil {
			return err
		}
		w.out.put("sizes", label, facts, sz)
	}
	rs, err := w.readSet(w.pastRecorded())
	if err != nil {
		return err
	}
	w.out.put("readset", label, facts, map[string]any{
		"past_valid": rs.pastValid, "past_recorded": rs.pastRecorded, "largest_team_members": rs.bigTeamMembers,
		"repositories": len(w.stream.repos), "people": len(w.stream.people), "teams": len(w.stream.teams), "changes": w.stream.changes,
		"simulated_day": w.stream.day,
	})
	w.pg.resetStatements(w.ctx)
	for _, op := range w.readOps(rs) {
		for _, c := range o.Callers {
			if op.onlyOne && c > 1 {
				continue
			}
			w.logf("measure %s: %s x%d", label, op.name, c)
			r := w.runOp(op, c, o.Duration)
			w.out.put("read", label, facts, r)
		}
	}
	if st, err := w.pg.topStatements(w.ctx, 15); err == nil && len(st) > 0 {
		w.out.put("statements", label, facts, st)
	}
	if w.pg != nil {
		plans, err := w.explainAnalyze(rs)
		if err != nil {
			return err
		}
		w.out.put("plans", label, facts, plans)
	}
	seq := &extraSeq{}
	extra, err := w.pg.extra(w.ctx)
	if err != nil {
		return err
	}
	seq.n.Store(extra)
	costs, err := w.resolveCosts(rs, seq)
	if err != nil {
		return err
	}
	w.out.put("resolve", label, facts, costs)
	applied := 0
	for _, level := range []string{"store", "resolver"} {
		for _, n := range o.Writers {
			w.logf("measure %s: %s writers x%d", label, level, n)
			var r applyResult
			if level == "store" {
				r = w.storeWriters(rs, n, o.Applies, seq)
			} else {
				r = w.resolverWriters(rs, n, o.Applies, seq)
			}
			applied += r.Applied
			w.out.put("apply", label, facts, r)
			if err := w.pg.addExtra(w.ctx, r.Applied); err != nil {
				return err
			}
		}
	}
	_ = applied
	return nil
}

// measureResolve is the part of measure that times the resolver, for a store
// that was measured before the resolver's database work was counted.
func (w *world) measureResolve(label string, facts int64) error {
	rs, err := w.readSet(w.pastRecorded())
	if err != nil {
		return err
	}
	seq := &extraSeq{}
	extra, err := w.pg.extra(w.ctx)
	if err != nil {
		return err
	}
	seq.n.Store(extra)
	costs, err := w.resolveCosts(rs, seq)
	if err != nil {
		return err
	}
	w.out.put("resolve", label, facts, costs)
	return nil
}

// pastRecorded is a record time before now, for bitemporal reads: the time of
// the previous checkpoint, or, without one, half way between the start of the
// load and now.
func (w *world) pastRecorded() time.Time {
	if n := len(w.checkpoints); n >= 2 {
		return w.checkpoints[n-2]
	}
	if w.loadStart.IsZero() {
		return w.now().Add(-time.Hour)
	}
	return w.loadStart.Add(w.now().Sub(w.loadStart) / 2)
}

func (w *world) runMeasure(f flags) error {
	done := int64(0)
	if w.pg != nil {
		var err error
		if done, err = w.pg.journal(w.ctx); err != nil {
			return err
		}
		extra, err := w.pg.extra(w.ctx)
		if err != nil {
			return err
		}
		done -= extra
	}
	for range done {
		w.stream.Next()
	}
	facts := int64(0)
	if w.pg != nil {
		var err error
		if facts, err = w.pg.factRows(w.ctx); err != nil {
			return err
		}
	}
	w.out.put("run", f.label, facts, w.environment(f))
	w.logf("measuring after %d events, %d fact rows", done, facts)
	label := f.label
	if label == "" {
		label = fmt.Sprintf("%d fact rows", facts)
	}
	if f.only == "resolve" {
		return w.measureResolve(label, facts)
	}
	return w.measure(label, facts, defaultSuite(f.quick))
}

// The load's checkpoints measure too.
func (w *world) runLoad(f flags, cps []int64) error {
	w.out.put("run", f.label, 0, w.environment(f))
	return w.load(loadOptions{Facts: f.facts, Events: f.events, Checkpoints: cps, Measure: func(w *world, facts int64) error {
		return w.measure(fmt.Sprintf("%d fact rows", facts), facts, defaultSuite(f.quick))
	}})
}
