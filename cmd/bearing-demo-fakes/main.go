// Command bearing-demo-fakes serves the fictional Acme org's GitHub,
// directory and Kubernetes fakes (internal/fakes) as one process for
// docs/demo.md and compose.yaml. It is a demo tool: it is not a shipped
// Bearing binary, its certificates and tokens are for demos only, and
// nothing outside the demo may depend on it.
//
// It writes a throwaway CA and certificates for the services "fakes" and
// "bearing" into -certs (kept if they are already there), serves the three
// fakes over HTTPS on -github, -directory and -kubernetes, and serves a
// plain-HTTP control endpoint on -control:
//
//	GET  /demo/steps                  the story's steps and which are played
//	POST /demo/advance?to=<step|time> play the story up to a step id or an
//	                                  RFC 3339 time, and send the signed
//	                                  webhook deliveries to -webhook-url
//
// The fakes accept the tokens the Bearing sources use, read from
// BEARING_SECRET_GITHUB_TOKEN, BEARING_SECRET_DIRECTORY_TOKEN and
// BEARING_SECRET_KUBERNETES_TOKEN, and sign deliveries with
// BEARING_SECRET_GITHUB_WEBHOOK. With -ready it checks that a running
// instance answers on -control and exits 0 if it does (the Compose
// health check; the image has no shell or curl).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"bearing.example/pkg/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := run(ctx, os.Args[1:], os.Getenv)
	if err != nil {
		telemetry.Logger("cmd/bearing-demo-fakes").Error("failed", "error", err.Error())
		os.Exit(1)
	}
}

// cleanups is the fakes.TB of a process: it runs its functions at exit.
type cleanups []func()

func (c *cleanups) Helper()          {}
func (c *cleanups) Cleanup(f func()) { *c = append(*c, f) }
func (c *cleanups) run() {
	for i := len(*c) - 1; i >= 0; i-- {
		(*c)[i]()
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("bearing-demo-fakes", flag.ContinueOnError)
	certs := fs.String("certs", "/certs", "directory for the demo CA and certificates")
	gh := fs.String("github", ":8441", "listen address of the GitHub fake (HTTPS)")
	dir := fs.String("directory", ":8442", "listen address of the directory fake (HTTPS)")
	kube := fs.String("kubernetes", ":8443", "listen address of the Kubernetes fake (HTTPS)")
	control := fs.String("control", ":8080", "listen address of the control endpoint (plain HTTP; publish it on loopback only)")
	webhook := fs.String("webhook-url", "", "where to send signed webhook deliveries, https://bearing:8443/webhooks/github-acme")
	ready := fs.Bool("ready", false, "check that an instance answers on -control and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ready {
		return checkReady(ctx, *control)
	}

	cfg := Config{
		GitHubToken: getenv("BEARING_SECRET_GITHUB_TOKEN"), DirectoryToken: getenv("BEARING_SECRET_DIRECTORY_TOKEN"),
		KubernetesToken: getenv("BEARING_SECRET_KUBERNETES_TOKEN"), WebhookSecret: getenv("BEARING_SECRET_GITHUB_WEBHOOK"),
		WebhookURL: *webhook,
	}
	for name, v := range map[string]string{
		"BEARING_SECRET_GITHUB_TOKEN": cfg.GitHubToken, "BEARING_SECRET_DIRECTORY_TOKEN": cfg.DirectoryToken,
		"BEARING_SECRET_KUBERNETES_TOKEN": cfg.KubernetesToken, "BEARING_SECRET_GITHUB_WEBHOOK": cfg.WebhookSecret,
	} {
		if v == "" {
			return fmt.Errorf("%s is not set: copy .env.example to .env", name)
		}
	}
	tlsCfg, pool, err := ensureCerts(*certs)
	if err != nil {
		return fmt.Errorf("certificates: %w", err)
	}
	cfg.TLS = tlsCfg
	cfg.WebhookClient = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	for addr, l := range map[string]*net.Listener{*gh: &cfg.GitHub, *dir: &cfg.Directory, *kube: &cfg.Kubernetes} {
		if *l, err = net.Listen("tcp", addr); err != nil {
			return err
		}
	}
	var tb cleanups
	defer tb.run()
	d := newDemo(&tb, cfg)

	srv := &http.Server{Addr: *control, Handler: d.control(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", *control)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	case err := <-errc:
		return err
	}
}

// checkReady asks the control endpoint of an instance on this host.
func checkReady(ctx context.Context, control string) error {
	_, port, err := net.SplitHostPort(control)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/demo/steps", http.NoBody)
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return errors.New("control endpoint: " + resp.Status)
	}
	return nil
}
