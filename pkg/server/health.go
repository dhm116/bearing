package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// healthTimeout bounds what a readiness check asks of the store.
const healthTimeout = 5 * time.Second

// Live reports whether the server is running and has not stopped for a
// failure. It looks at nothing outside the process, so a probe that restarts
// a server on failure does not restart it for a database outage.
func (s *Server) Live() error {
	if err, _ := s.failed.Load().(error); err != nil {
		return err
	}
	if !s.running.Load() {
		return errors.New("server: not running")
	}
	return nil
}

// Ready reports whether the server can do its work now: it is live, and the
// store, the log and the instance lock answer.
func (s *Server) Ready(ctx context.Context) error {
	if err := s.Live(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := s.storeDown(ctx); err != nil {
		return err
	}
	if s.cfg.Lock != nil {
		if err := s.cfg.Lock.Held(ctx); err != nil {
			return err
		}
	}
	return nil
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
