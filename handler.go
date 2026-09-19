package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// Handler serves converted images. It is safe for concurrent use.
type Handler struct {
	cfg            *Config
	cache          *ImageCache
	fetcher        *Fetcher
	conv           *Converter
	log            *logrus.Logger
	group          singleflight.Group
	upstreamScheme string
}

func NewHandler(cfg *Config, cache *ImageCache, fetcher *Fetcher, conv *Converter, log *logrus.Logger) *Handler {
	return &Handler{
		cfg:            cfg,
		cache:          cache,
		fetcher:        fetcher,
		conv:           conv,
		log:            log,
		upstreamScheme: "https://",
	}
}

// hostOnly strips an optional :port from a Host header value.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// methodLabel bounds the Prometheus method label to a fixed set.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead:
		return m
	}
	return "other"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	defer func() {
		code := strconv.Itoa(rec.Status())
		method := methodLabel(r.Method)
		requestsTotal.WithLabelValues(code, method).Inc()
		responseDuration.WithLabelValues(code, method).Observe(time.Since(start).Seconds())
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(rec, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	origin, ok := h.cfg.AllowedHosts[hostOnly(r.Host)]
	if !ok {
		invalidHostsCount.Inc()
		http.Error(rec, "Host not allowed", http.StatusForbidden)
		return
	}

	quality := resolveQuality(r.Header.Get("x-webp-quality"), h.cfg.WebP.Quality)
	imageURL := buildImageURL(h.upstreamScheme, origin, r.URL.Path, r.URL.RawQuery)
	key := cacheKey(imageURL, quality)
	useCache := h.cfg.Cache.CacheEnabled && r.Header.Get(h.cfg.Cache.NoCacheHeader) != "true"

	if useCache {
		if e, hit := h.cache.Get(key); hit {
			cacheHitsTotal.Inc()
			h.writeEntry(rec, r, e)
			return
		}
		if code, hit := h.cache.GetNegative(key); hit {
			negativeHitsTotal.Inc()
			http.Error(rec, http.StatusText(code), code)
			return
		}
		cacheMissesTotal.Inc()
	}

	leader := false
	v, err, shared := h.group.Do(key, func() (any, error) {
		leader = true
		return h.produce(imageURL, quality)
	})
	if shared && !leader {
		coalescedTotal.Inc()
	}
	if err != nil {
		var us *UpstreamStatusError
		if errors.As(err, &us) && (us.Code == http.StatusNotFound || us.Code == http.StatusGone) {
			if useCache {
				h.cache.SetNegative(key, http.StatusNotFound)
			}
			http.Error(rec, "Not Found", http.StatusNotFound)
			return
		}
		errorsTotal.Inc()
		h.log.WithFields(logrus.Fields{"url": imageURL, "error": err.Error()}).Error("image processing failed")
		http.Error(rec, "Bad Gateway", http.StatusBadGateway)
		return
	}

	e := v.(Entry)
	if useCache {
		h.cache.Set(key, e)
	}
	h.writeEntry(rec, r, e)
}

// produce fetches and converts one image. It runs under a detached context
// so that a client disconnecting does not fail the other coalesced waiters.
func (h *Handler) produce(imageURL string, quality int) (Entry, error) {
	timeout := time.Duration(h.cfg.HTTPClient.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()

	body, err := h.fetcher.Fetch(ctx, imageURL)
	if err != nil {
		return Entry{}, err
	}
	defer body.Release()

	data, err := h.conv.ToWebP(ctx, body.Bytes(), quality)
	if err != nil {
		return Entry{}, err
	}
	return NewEntry(data), nil
}

func (h *Handler) writeEntry(w http.ResponseWriter, r *http.Request, e Entry) {
	hdr := w.Header()
	hdr.Set("ETag", e.ETag)
	hdr.Set("Vary", "x-webp-quality")
	if cc := h.cfg.HTTPServer.CacheControl; cc != "" {
		hdr.Set("Cache-Control", cc)
	}
	if r.Header.Get("If-None-Match") == e.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hdr.Set("Content-Type", "image/webp")
	hdr.Set("Content-Length", strconv.Itoa(len(e.Data)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Write(e.Data)
}
