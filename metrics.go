package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "ngax_http_requests_total", Help: "Total number of HTTP requests."},
		[]string{"status_code", "method"},
	)
	responseDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ngax_http_response_duration_seconds",
			Help:    "Histogram of HTTP response durations.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"status_code", "method"},
	)
	cacheHitsTotal                 = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_cache_hits_total", Help: "Total number of cache hits."})
	cacheMissesTotal               = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_cache_misses_total", Help: "Total number of cache misses."})
	negativeHitsTotal              = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_negative_cache_hits_total", Help: "Requests answered from the negative (404) cache."})
	coalescedTotal                 = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_coalesced_requests_total", Help: "Cache misses that waited on an in-flight fetch for the same key instead of fetching."})
	errorsTotal                    = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_http_errors_total", Help: "Total number of HTTP errors."})
	totalImageSizeBeforeConversion = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_total_image_size_before_conversion_bytes", Help: "Total size of images before conversion in bytes."})
	totalImageSizeAfterConversion  = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_total_image_size_after_conversion_bytes", Help: "Total size of images after conversion in bytes."})
	invalidHostsCount              = prometheus.NewCounter(prometheus.CounterOpts{Name: "ngax_invalid_hosts_count", Help: "Count of unauthorized host access attempts."})
)

func init() {
	prometheus.MustRegister(
		requestsTotal, responseDuration, cacheHitsTotal, cacheMissesTotal, negativeHitsTotal,
		coalescedTotal, errorsTotal, totalImageSizeBeforeConversion, totalImageSizeAfterConversion,
		invalidHostsCount,
	)
}

// cacheBytesCollector builds the GaugeFunc collector that reports
// ngax_cache_bytes, read from the cache's own admitted-cost counters at
// scrape time rather than written on the request hot path. Extracted as its
// own function so it can be tested against a fresh registry, since
// MustRegister on the default registry can only run once per process.
func cacheBytesCollector(c *ImageCache) prometheus.Collector {
	return prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{Name: "ngax_cache_bytes", Help: "Approximate bytes held by the image cache."},
		func() float64 { return float64(c.Bytes()) },
	)
}

// registerCacheBytes exposes ngax_cache_bytes as a gauge read at scrape time
// from the cache's own admitted-cost counters, keeping the write path free of
// metric bookkeeping. Call it once for the process-wide cache.
func registerCacheBytes(c *ImageCache) {
	prometheus.MustRegister(cacheBytesCollector(c))
}

// statusRecorder captures the status code written by a handler so metrics
// can be recorded with the real outcome.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
