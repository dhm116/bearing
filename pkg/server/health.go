package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const (
	// healthTimeout bounds what a readiness check asks of the store.
	healthTimeout = 5 * time.Second
	// readyCacheTTL is how long a readiness answer is reused. The endpoints
	// take no credentials, so the store sees at most one check a second
	// however many probes arrive.
	readyCacheTTL = time.Second
)

// Live reports whether the server is running and has not stopped for a
// failure. It looks at nothing outside the process, so a probe that restarts
// a server on failure does not restart it for a database outage.
func (s *Server) Live() error {
	if err := s.failed.Load(); err != nil {
		return *err
	}
	if !s.running.Load() {
		return errors.New("server: not running")
	}
	return nil
}

// Ready reports whether the server can do its work now: it is live, the
// store and the log answer, and the last check of the instance lock held. The
// lock is checked by the server's own loop, not here, so probes never share
// its connection, and a probe that goes away cannot end the lock's session.
func (s *Server) Ready(ctx context.Context) error {
	if err := s.Live(); err != nil {
		return err
	}
	if err := s.lockErr.Load(); err != nil {
		return *err
	}
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	now := s.clk.Now()
	if !s.readyAt.IsZero() && now.Sub(s.readyAt) < readyCacheTTL && !now.Before(s.readyAt) {
		return s.readyErr
	}
	// A prober that disconnects must not turn a healthy answer into an error
	// that the next prober is given.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthTimeout)
	defer cancel()
	s.readyAt, s.readyErr = now, s.storeDown(ctx)
	return s.readyErr
}

// HealthHandler serves GET /healthz (live) and GET /readyz (ready). The
// answer is the status code and a one-word body: a probe is not told why, since
// the endpoints take no credentials.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	answer := func(check func(*http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if check(r) != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("unavailable\n"))
				return
			}
			_, _ = w.Write([]byte("ok\n"))
		}
	}
	mux.Handle("GET /healthz", answer(func(*http.Request) error { return s.Live() }))
	mux.Handle("GET /readyz", answer(func(r *http.Request) error { return s.Ready(r.Context()) }))
	return mux
}
