package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/h2non/bimg"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type Config struct {
	WebP struct {
		Quality      int  `mapstructure:"quality"`
		Lossless     bool `mapstructure:"lossless"`
		NearLossless int  `mapstructure:"near_lossless"`
	} `mapstructure:"webp"`
	Cache struct {
		// ExpirationMinutes int    `mapstructure:"expiration_minutes"`
		CacheEnabled  bool   `mapstructure:"cache_enabled"`
		NoCacheHeader string `mapstructure:"nocache_header"`
		LruCache      int    `mapstructure:"lru_cache"`
	} `mapstructure:"cache"`
	Concurrency struct {
		MaxGoroutines int `mapstructure:"max_goroutines"`
	} `mapstructure:"concurrency"`
	HTTPClient struct {
		TimeoutSeconds        int `mapstructure:"timeout_seconds"`
		DialTimeoutSeconds    int `mapstructure:"dial_timeout_seconds"`
		KeepAlive             int `mapstructure:"keep_alive"`
		TLSHandshakeTimeout   int `mapstructure:"TLS_handshake_timeout"`
		ResponseHeaderTimeout int `mapstructure:"response_header_timeout"`
		ExpectContinueTimeout int `mapstructure:"expect_continue_timeout"`
	} `mapstructure:"http_client"`
	AllowedHosts map[string]string `mapstructure:"allowed_hosts"`
	Limits       struct {
		MaxImageBytes int64 `mapstructure:"max_image_bytes"`
	} `mapstructure:"limits"`
	Exporter struct {
		BindIP   string `mapstructure:"bind_ip"`
		Port     int    `mapstructure:"port"`
		User     string `mapstructure:"user"`
		Password string `mapstructure:"password"`
	} `mapstructure:"exporter"`
	HTTPServer struct {
		BindIP string `mapstructure:"bind_ip"`
		Port   int    `mapstructure:"port"`
	} `mapstructure:"http_server"`
}

// defaultMaxImageBytes caps the size of an upstream image when limits.max_image_bytes is unset.
const defaultMaxImageBytes = 20 << 20

// upstreamScheme is the scheme used for upstream fetches; overridden in tests.
var upstreamScheme = "https://"

var (
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ngax_http_requests_total",
			Help: "Total number of HTTP requests.",
		},
		[]string{"status_code", "method"},
	)

	responseDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "ngax_http_response_duration_seconds",
			Help: "Histogram of HTTP response durations.",
		},
		[]string{"status_code", "method"},
	)

	cacheHitsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ngax_cache_hits_total",
			Help: "Total number of cache hits.",
		},
	)

	cacheMissesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ngax_cache_misses_total",
			Help: "Total number of cache misses.",
		},
	)

	errorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ngax_http_errors_total",
			Help: "Total number of HTTP errors.",
		},
	)

	totalImageSizeBeforeConversion = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ngax_total_image_size_before_conversion_bytes",
			Help: "Total size of images before conversion in bytes.",
		},
	)

	// Total size of images after conversion
	totalImageSizeAfterConversion = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ngax_total_image_size_after_conversion_bytes",
			Help: "Total size of images after conversion in bytes.",
		},
	)

	invalidHostsCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ngax_invalid_hosts_count",
			Help: "Count of unauthorized host access attempts.",
		},
	)

	config     Config
	imgCache   *lru.TwoQueueCache[string, []byte]
	logger     = newLogger()
	httpClient *http.Client
	semaphore  chan struct{}
)

func init() {
	prometheus.MustRegister(requestsTotal)
	prometheus.MustRegister(responseDuration)
	prometheus.MustRegister(cacheHitsTotal)
	prometheus.MustRegister(cacheMissesTotal)
	prometheus.MustRegister(errorsTotal)
	prometheus.MustRegister(totalImageSizeBeforeConversion)
	prometheus.MustRegister(totalImageSizeAfterConversion)
	prometheus.MustRegister(invalidHostsCount)
}

func newLogger() *logrus.Logger {
	l := logrus.New()
	l.Out = os.Stdout
	l.Level = logrus.DebugLevel
	l.Formatter = &logrus.JSONFormatter{}
	return l
}

// loadConfig reads config.yaml from dir into the global config.
func loadConfig(dir string) error {
	// Hostnames are map keys in allowed_hosts, so the default "." key
	// delimiter must not be used or viper would split them into nested maps.
	v := viper.NewWithOptions(viper.KeyDelimiter("::"))
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(dir)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	if err := v.Unmarshal(&config); err != nil {
		return fmt.Errorf("decoding config: %w", err)
	}
	return nil
}

// setupRuntime builds the cache, HTTP client and semaphore from the loaded config.
func setupRuntime() error {
	var err error
	imgCache, err = lru.New2Q[string, []byte](config.Cache.LruCache)
	if err != nil {
		return fmt.Errorf("creating cache: %w", err)
	}

	httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   time.Duration(config.HTTPClient.DialTimeoutSeconds) * time.Second,
				KeepAlive: time.Duration(config.HTTPClient.KeepAlive) * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   time.Duration(config.HTTPClient.TLSHandshakeTimeout) * time.Second,
			ResponseHeaderTimeout: time.Duration(config.HTTPClient.ResponseHeaderTimeout) * time.Second,
			ExpectContinueTimeout: time.Duration(config.HTTPClient.ExpectContinueTimeout) * time.Second,
			MaxIdleConns:          100,
		},
		Timeout: time.Second * time.Duration(config.HTTPClient.TimeoutSeconds),
	}

	if config.Concurrency.MaxGoroutines <= 0 {
		config.Concurrency.MaxGoroutines = 1
	}
	semaphore = make(chan struct{}, config.Concurrency.MaxGoroutines)
	if config.Limits.MaxImageBytes <= 0 {
		config.Limits.MaxImageBytes = defaultMaxImageBytes
	}
	return nil
}

func main() {
	if err := loadConfig("."); err != nil {
		logger.Fatal(err)
	}
	if err := setupRuntime(); err != nil {
		logger.Fatal(err)
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf("%v:%v", config.HTTPServer.BindIP, config.HTTPServer.Port),
		Handler: newRouter(),
	}

	metricsRouter := mux.NewRouter()
	metricsRouter.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:    fmt.Sprintf("%v:%v", config.Exporter.BindIP, config.Exporter.Port),
		Handler: basicAuthMiddleware(metricsRouter),
	}

	// pprof is registered on the default mux by the net/http/pprof import and
	// is only reachable from the local machine.
	pprofSrv := &http.Server{Addr: "localhost:6060", Handler: http.DefaultServeMux}

	errChan := make(chan error, 3)
	serve := func(name string, s *http.Server) {
		logger.Infof("%s listening on http://%s", name, s.Addr)
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("%s: %w", name, err)
		}
	}
	go serve("server", srv)
	go serve("exporter", metricsSrv)
	go serve("pprof", pprofSrv)

	gracefulShutdown(errChan, srv, metricsSrv, pprofSrv)
}

// newRouter builds the public router. Fixed routes are registered before the
// catch-all so they are never subject to the host allowlist.
func newRouter() *mux.Router {
	router := mux.NewRouter()
	router.HandleFunc("/health", healthCheckHandler)
	router.HandleFunc("/{image:.*}", handleRequest)
	return router
}

// statusRecorder captures the status code written by a handler.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	defer func() {
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		code := strconv.Itoa(rec.status)
		requestsTotal.WithLabelValues(code, r.Method).Inc()
		responseDuration.WithLabelValues(code, r.Method).Observe(time.Since(startTime).Seconds())
	}()

	remoteHost := r.Host
	if remoteHost == "" {
		http.Error(rec, "Host header is missing", http.StatusBadRequest)
		return
	}

	// Map the incoming host to the upstream host that actually holds the image.
	allowedHost, exists := config.AllowedHosts[remoteHost]
	if !exists {
		invalidHostsCount.Inc()
		logger.Warn("Unauthorized access attempt from host: ", remoteHost)
		http.Error(rec, "Host not allowed", http.StatusForbidden)
		return
	}

	imageURL := buildImageURL(allowedHost, r.URL.Path, r.URL.RawQuery)

	useCache := config.Cache.CacheEnabled && r.Header.Get(config.Cache.NoCacheHeader) != "true"
	if useCache {
		if cachedImage, found := imgCache.Get(imageURL); found {
			logger.Debug("Cache hit for URL: ", imageURL)
			cacheHitsTotal.Inc()
			writeWebP(rec, cachedImage)
			return
		}
		cacheMissesTotal.Inc()
		logger.Debug("Cache miss for URL: ", imageURL)
	}

	semaphoreWaitStart := time.Now()
	semaphore <- struct{}{}
	defer func() { <-semaphore }()
	logger.WithFields(logrus.Fields{
		"semaphoreWaitDuration": time.Since(semaphoreWaitStart),
	}).Debug("Semaphore slot acquired")

	quality := resolveQuality(r.Header.Get("x-webp-quality"), config.WebP.Quality)
	newImage, err := fetchAndConvert(imageURL, quality)
	if err != nil {
		logger.WithFields(logrus.Fields{"error": err.Error(), "url": imageURL}).Error("Error processing image")
		http.Error(rec, "Error processing image", http.StatusBadGateway)
		errorsTotal.Inc()
		return
	}

	if useCache {
		imgCache.Add(imageURL, newImage)
	}
	writeWebP(rec, newImage)
}

// fetchAndConvert downloads imageURL and converts it to WebP at the given quality.
func fetchAndConvert(imageURL string, quality int) ([]byte, error) {
	resp, err := httpClient.Get(imageURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error from remote host: %s", resp.Status)
	}

	if !isSupportedImageFormat(resp.Header.Get("Content-Type")) {
		return nil, fmt.Errorf("unsupported image format %q", resp.Header.Get("Content-Type"))
	}

	limit := config.Limits.MaxImageBytes
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading image body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("image exceeds size limit of %d bytes", limit)
	}
	totalImageSizeBeforeConversion.Add(float64(len(body)))

	options := bimg.Options{
		Quality:  quality,
		Lossless: config.WebP.Lossless,
		Type:     bimg.WEBP,
	}
	newImage, err := bimg.NewImage(body).Process(options)
	if err != nil {
		return nil, fmt.Errorf("converting image: %w", err)
	}
	totalImageSizeAfterConversion.Add(float64(len(newImage)))
	return newImage, nil
}

func writeWebP(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func isSupportedImageFormat(contentType string) bool {
	supportedFormats := []string{"jpeg", "jpg", "png", "gif", "bmp", "webp", "tiff"}
	for _, format := range supportedFormats {
		if strings.Contains(contentType, format) {
			return true
		}
	}
	return false
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// gracefulShutdown waits for a termination signal or a listener failure, then
// drains every server before returning.
func gracefulShutdown(errChan <-chan error, servers ...*http.Server) {
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stopChan:
		logger.Infof("Received %s, shutting down...", sig)
	case err := <-errChan:
		logger.Errorf("Listener failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, s := range servers {
		if err := s.Shutdown(ctx); err != nil {
			logger.Errorf("Forced shutdown of %s: %v", s.Addr, err)
		}
	}
	logger.Info("Servers stopped")
}

func basicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != config.Exporter.User || pass != config.Exporter.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// resolveQuality returns the WebP quality to use: the header value when it is
// a valid integer in 1..100 (values above 100 are clamped), otherwise def.
func resolveQuality(header string, def int) int {
	q, err := strconv.Atoi(header)
	if err != nil || q <= 0 {
		return def
	}
	if q > 100 {
		return 100
	}
	return q
}

// buildImageURL builds the upstream URL, keeping the query string so that
// distinct variants of an image are fetched and cached separately.
func buildImageURL(host, path, rawQuery string) string {
	u := upstreamScheme + host + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}
