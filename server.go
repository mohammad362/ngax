package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

func newLogger(level string) *logrus.Logger {
	l := logrus.New()
	l.Out = os.Stdout
	l.Formatter = &logrus.JSONFormatter{}
	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		lvl = logrus.InfoLevel
	}
	l.Level = lvl
	return l
}

// newRouter registers fixed routes before the catch-all so they are never
// subject to the host allowlist.
func newRouter(images http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthCheckHandler)
	mux.HandleFunc("HEAD /health", healthCheckHandler)
	mux.Handle("/", images)
	return mux
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func newPublicServer(cfg *Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTPServer.BindIP, cfg.HTTPServer.Port),
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(cfg.HTTPServer.ReadHeaderTimeoutSeconds) * time.Second,
		IdleTimeout:       time.Duration(cfg.HTTPServer.IdleTimeoutSeconds) * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func newMetricsServer(cfg *Config) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	return &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Exporter.BindIP, cfg.Exporter.Port),
		Handler:           basicAuthMiddleware(cfg.Exporter.User, cfg.Exporter.Password, mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// newPprofServer exposes net/http/pprof (registered on the default mux by
// the blank import) to the local machine only.
func newPprofServer() *http.Server {
	return &http.Server{
		Addr:              "localhost:6060",
		Handler:           http.DefaultServeMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func basicAuthMiddleware(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		// Constant-time on both fields so a timing oracle cannot recover
		// either the user name or the password byte by byte.
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// runServers starts every server, then blocks until SIGINT/SIGTERM or a
// listener failure, and shuts them all down.
func runServers(log *logrus.Logger, servers ...*http.Server) {
	errChan := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			log.Infof("listening on http://%s", s.Addr)
			if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("%s: %w", s.Addr, err)
			}
		}(s)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Infof("received %s, shutting down", sig)
	case err := <-errChan:
		log.Errorf("listener failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(ctx); err != nil {
			log.Errorf("forced shutdown of %s: %v", s.Addr, err)
		}
	}
	log.Info("servers stopped")
}
