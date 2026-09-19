package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"
)

type handlerFixture struct {
	h        *Handler
	cache    *ImageCache
	origin   *httptest.Server
	fetches  atomic.Int32
	gate     chan struct{} // when non-nil, origin blocks until closed
	notFound atomic.Bool
}

func newFixture(t *testing.T) *handlerFixture {
	t.Helper()
	fx := &handlerFixture{}
	data := pngBytes(t)
	fx.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.fetches.Add(1)
		if fx.gate != nil {
			<-fx.gate
		}
		if fx.notFound.Load() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	t.Cleanup(fx.origin.Close)

	cfg := &Config{}
	cfg.AllowedHosts = map[string]string{"cdn.test": strings.TrimPrefix(fx.origin.URL, "http://")}
	cfg.applyDefaults()
	cfg.Cache.CacheEnabled = true
	cfg.Cache.MaxBytes = 8 << 20
	cfg.Cache.NegativeTTLSeconds = 60
	cfg.HTTPClient.TimeoutSeconds = 2

	var err error
	fx.cache, err = NewImageCache(cfg.Cache.MaxBytes, cfg.NegativeTTL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fx.cache.Close)

	log := logrus.New()
	log.Out = &bytes.Buffer{}
	fx.h = NewHandler(cfg, fx.cache, NewFetcher(newHTTPClient(cfg), cfg.Limits.MaxImageBytes, 16), NewConverter(2, false), log)
	fx.h.upstreamScheme = "http://"
	return fx
}

func (fx *handlerFixture) get(path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "cdn.test"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerServesWebPWithETagAndCacheControl(t *testing.T) {
	fx := newFixture(t)
	rec := fx.get("/a.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !isWebP(rec.Body.Bytes()) {
		t.Fatal("body is not WebP")
	}
	if rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("Content-Type %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Vary") != "x-webp-quality" {
		t.Fatalf("Vary %q", rec.Header().Get("Vary"))
	}
}

func TestHandlerReturns304WhenETagMatches(t *testing.T) {
	fx := newFixture(t)
	first := fx.get("/a.png", nil)
	etag := first.Header().Get("ETag")
	fx.cache.Wait()
	second := fx.get("/a.png", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatal("304 must have empty body")
	}
	if second.Header().Get("Vary") != "x-webp-quality" {
		t.Fatalf("304 Vary %q", second.Header().Get("Vary"))
	}
}

func TestHandlerServesSecondRequestFromCache(t *testing.T) {
	fx := newFixture(t)
	fx.get("/a.png", nil)
	fx.cache.Wait()
	before := testutil.ToFloat64(cacheHitsTotal)
	fx.get("/a.png", nil)
	if fx.fetches.Load() != 1 {
		t.Fatalf("want 1 origin fetch, got %d", fx.fetches.Load())
	}
	if testutil.ToFloat64(cacheHitsTotal)-before != 1 {
		t.Fatal("cache hit counter did not increase")
	}
}

func TestHandlerCoalescesConcurrentMisses(t *testing.T) {
	fx := newFixture(t)
	fx.gate = make(chan struct{})
	const clients = 50
	var wg sync.WaitGroup
	codes := make([]int, clients)
	before := testutil.ToFloat64(coalescedTotal)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = fx.get("/hot.png", nil).Code
		}(i)
	}
	time.Sleep(150 * time.Millisecond) // let every client reach singleflight
	close(fx.gate)
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("client %d got %d", i, c)
		}
	}
	if fx.fetches.Load() != 1 {
		t.Fatalf("want exactly 1 origin fetch for %d concurrent clients, got %d", clients, fx.fetches.Load())
	}
	if delta := testutil.ToFloat64(coalescedTotal) - before; delta != clients-1 {
		t.Fatalf("want %d coalesced followers, got %v", clients-1, delta)
	}
}

func TestHandlerBoundsMethodLabel(t *testing.T) {
	fx := newFixture(t)
	before := testutil.ToFloat64(requestsTotal.WithLabelValues("405", "other"))
	req := httptest.NewRequest("BOGUS", "/a.png", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
	if testutil.ToFloat64(requestsTotal.WithLabelValues("405", "other"))-before != 1 {
		t.Fatal("unknown method must be recorded under the fixed label \"other\"")
	}
	if testutil.ToFloat64(requestsTotal.WithLabelValues("405", "BOGUS")) != 0 {
		t.Fatal("raw method token must never become a label value")
	}
}

func TestHandlerCacheKeyIncludesQuality(t *testing.T) {
	fx := newFixture(t)
	a := fx.get("/a.png", map[string]string{"x-webp-quality": "20"})
	fx.cache.Wait()
	b := fx.get("/a.png", map[string]string{"x-webp-quality": "95"})
	if a.Header().Get("ETag") == b.Header().Get("ETag") {
		t.Fatal("different qualities served identical bytes; cache key ignores quality")
	}
	if fx.fetches.Load() != 2 {
		t.Fatalf("want 2 fetches, got %d", fx.fetches.Load())
	}
}

func TestHandlerNegativeCachesOrigin404(t *testing.T) {
	fx := newFixture(t)
	fx.notFound.Store(true)
	if rec := fx.get("/gone.png", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	fx.cache.Wait()
	before := testutil.ToFloat64(negativeHitsTotal)
	if rec := fx.get("/gone.png", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 from negative cache, got %d", rec.Code)
	}
	if fx.fetches.Load() != 1 {
		t.Fatalf("want 1 origin fetch, got %d", fx.fetches.Load())
	}
	if testutil.ToFloat64(negativeHitsTotal)-before != 1 {
		t.Fatal("negative hit counter did not increase")
	}
}

func TestHandlerRejectsUnknownHost(t *testing.T) {
	fx := newFixture(t)
	before := testutil.ToFloat64(requestsTotal.WithLabelValues("403", "GET"))
	req := httptest.NewRequest(http.MethodGet, "/a.png", nil)
	req.Host = "not-allowed.test"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if testutil.ToFloat64(requestsTotal.WithLabelValues("403", "GET"))-before != 1 {
		t.Fatal("403 was not recorded in requestsTotal")
	}
}

func TestHandlerStripsPortFromHost(t *testing.T) {
	fx := newFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/a.png", nil)
	req.Host = "cdn.test:8080"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for host with port, got %d", rec.Code)
	}
}

func TestHandlerRejectsNonGetMethods(t *testing.T) {
	fx := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/a.png", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

func TestHandlerHeadReturnsHeadersOnly(t *testing.T) {
	fx := newFixture(t)
	fx.get("/a.png", nil)
	fx.cache.Wait()
	req := httptest.NewRequest(http.MethodHead, "/a.png", nil)
	req.Host = "cdn.test"
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: code=%d body=%d content-length=%q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
}

func TestHandlerSkipsCacheWhenDisabled(t *testing.T) {
	fx := newFixture(t)
	fx.h.cfg.Cache.CacheEnabled = false
	fx.get("/a.png", nil)
	fx.get("/a.png", nil)
	fx.cache.Wait()
	if fx.fetches.Load() != 2 {
		t.Fatalf("want 2 fetches with cache disabled, got %d", fx.fetches.Load())
	}
	if _, ok := fx.cache.Get(cacheKey(fx.h.upstreamScheme+fx.h.cfg.AllowedHosts["cdn.test"]+"/a.png", 75)); ok {
		t.Fatal("cache must stay empty when disabled")
	}
}

func TestHandlerNoCacheHeaderBypassesCache(t *testing.T) {
	fx := newFixture(t)
	fx.get("/a.png", nil)
	fx.cache.Wait()
	fx.get("/a.png", map[string]string{"X-No-Cache": "true"})
	if fx.fetches.Load() != 2 {
		t.Fatalf("want 2 fetches, got %d", fx.fetches.Load())
	}
}

func TestHostOnly(t *testing.T) {
	for in, want := range map[string]string{"a.test": "a.test", "a.test:8080": "a.test", "[::1]:80": "::1"} {
		if got := hostOnly(in); got != want {
			t.Errorf("%q: want %q, got %q", in, want, got)
		}
	}
}
