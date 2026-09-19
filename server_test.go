package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestHealthIgnoresHostAllowlist(t *testing.T) {
	fx := newFixture(t)
	router := newRouter(fx.h)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Host = "not-allowed.test"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
		t.Fatalf("want 200 OK, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestMetricsNotServedOnPublicPort(t *testing.T) {
	fx := newFixture(t)
	router := newRouter(fx.h)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if bytes.Contains(rec.Body.Bytes(), []byte("ngax_http_requests_total")) {
		t.Fatal("metrics exposed on the public router")
	}
}

func TestMetricsServerRequiresBasicAuth(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	cfg.Exporter.User, cfg.Exporter.Password = "foo", "bar"
	srv := newMetricsServer(cfg)

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without credentials, got %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.SetBasicAuth("foo", "bar")
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("ngax_")) {
		t.Fatalf("want 200 with metrics, got %d", rec.Code)
	}
}

func TestPublicServerHasTimeouts(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	srv := newPublicServer(cfg, http.NotFoundHandler())
	if srv.ReadHeaderTimeout == 0 || srv.IdleTimeout == 0 || srv.MaxHeaderBytes == 0 {
		t.Fatalf("timeouts unset: rht=%v idle=%v mhb=%d", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}
}

func TestNewLoggerParsesLevel(t *testing.T) {
	if newLogger("debug").Level != logrus.DebugLevel {
		t.Fatal("debug level not applied")
	}
	if newLogger("nonsense").Level != logrus.InfoLevel {
		t.Fatal("invalid level must fall back to info")
	}
}
