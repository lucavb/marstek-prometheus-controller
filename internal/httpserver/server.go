// Package httpserver provides the HTTP server that exposes /metrics,
// /healthz, and /readyz for the Marstek controller.
//
// /metrics  — Prometheus scrape endpoint backed by the controller's private registry.
// /healthz  — Liveness: always 200 while the process is up.
// /readyz   — Readiness: 200 once the controller has completed at least one full
//
//	control step that successfully read Prometheus AND observed a
//	live device status from MQTT (which implies a connected broker
//	session). A step that was legitimately suppressed by deadband,
//	hold-time, or command-delta still counts, because those all
//	happen after both Prom and MQTT health have been verified.
package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ReadyChecker is implemented by anything that can report whether the
// controller has reached a ready state.
type ReadyChecker interface {
	Ready() bool
}

// Server is the HTTP server.
type Server struct {
	srv *http.Server
}

// New creates an HTTP server bound to addr. It mounts the three endpoints
// against the provided registry and readyChecker.
func New(addr string, reg *prometheus.Registry, ready ReadyChecker) *Server {
	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: false,
	}))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
		}
	})

	return &Server{
		srv: &http.Server{
			Addr:         addr,
			Handler:      mux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  30 * time.Second,
			BaseContext: func(_ net.Listener) context.Context {
				return context.Background()
			},
		},
	}
}

// ListenAndServe starts the server. It blocks until the server is stopped.
// Returns nil if the server was stopped via Shutdown.
func (s *Server) ListenAndServe() error {
	err := s.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server with a 5s deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
