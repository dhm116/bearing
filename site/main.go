// Command site builds Bearing's website from the repository: the
// specification and ADRs straight from docs/, the roadmap from
// roadmap.yaml and GitHub milestones, and the hand-written pages in
// templates/. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(os.Args[1:], log); err != nil {
		log.Error("site build failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string, log *slog.Logger) error {
	fl := flag.NewFlagSet("site", flag.ContinueOnError)
	siteDir := fl.String("site", ".", "directory holding site.yaml, roadmap.yaml, templates/ and static/")
	root := fl.String("root", "..", "repository root")
	out := fl.String("out", "_site", "output directory (cleared first)")
	base := fl.String("base", "/", "URL path the site is served under, e.g. /bearing/")
	offline := fl.Bool("offline", false, "don't read milestones and issues from GitHub")
	strict := fl.Bool("strict", false, "fail on broken links or when GitHub can't be read")
	serve := fl.String("serve", "", "after building, serve the site on this address, e.g. localhost:8080")
	if err := fl.Parse(args); err != nil {
		return err
	}

	cfg, err := LoadConfig(filepath.Join(*siteDir, "site.yaml"))
	if err != nil {
		return err
	}
	rf, err := LoadRoadmap(filepath.Join(*siteDir, "roadmap.yaml"))
	if err != nil {
		return err
	}

	var gh *GitHubData
	if !*offline {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		// GITHUB_TOKEN only raises the rate limit; the repository is public.
		gh, err = (&GitHub{Repo: cfg.Repo, Token: os.Getenv("GITHUB_TOKEN")}).Fetch(ctx)
		cancel()
		if err != nil {
			if *strict {
				return fmt.Errorf("site: read GitHub: %w", err)
			}
			log.Warn("GitHub unavailable; roadmap shows statuses from roadmap.yaml only", "err", err)
			gh = nil
		}
	}

	b := &Builder{
		Root: *root, SiteDir: *siteDir, Out: *out, Base: normalizeBase(*base),
		Config: cfg, Roadmap: BuildRoadmap(rf, gh, cfg.Repo), Log: log,
		Build: BuildInfo{Commit: commit(*root), Time: time.Now().UTC()},
	}
	if err := b.Run(); err != nil {
		return err
	}
	for _, p := range b.Problems() {
		log.Warn(p)
	}
	if *strict && len(b.Problems()) > 0 {
		return fmt.Errorf("site: %d problems", len(b.Problems()))
	}
	log.Info("site built", "out", *out, "base", b.Base, "github", gh != nil)

	if *serve != "" {
		log.Info("serving", "url", "http://"+*serve+b.Base)
		mux := http.NewServeMux()
		mux.Handle(b.Base, http.StripPrefix(strings.TrimSuffix(b.Base, "/"), http.FileServer(http.Dir(*out))))
		srv := &http.Server{Addr: *serve, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

func normalizeBase(b string) string {
	b = "/" + strings.Trim(b, "/") + "/"
	if b == "//" {
		return "/"
	}
	return b
}

// commit is the commit being built: GITHUB_SHA in Actions, else git's HEAD.
func commit(root string) string {
	if sha := os.Getenv("GITHUB_SHA"); sha != "" {
		return sha
	}
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
