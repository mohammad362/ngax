package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/h2non/bimg"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// upstreamScheme is the scheme used for upstream fetches; overridden in tests.
var upstreamScheme = "https://"

var (
	imgCache   *lru.TwoQueueCache[string, []byte]
	logger     = newLogger()
	httpClient *http.Client
	semaphore  chan struct{}
)

func newLogger() *logrus.Logger {
	l := logrus.New()
	l.Out = os.Stdout
	l.Level = logrus.DebugLevel
	l.Formatter = &logrus.JSONFormatter{}
	return l
}

// setupRuntime builds the cache, HTTP client and semaphore from the loaded config.
func setupRuntime() error {
	var err error
	imgCache, err = lru.New2Q[string, []byte](1024)
	if err != nil {
		return fmt.Errorf("creating cache: %w", err)
	}

	httpClient = newHTTPClient(&config)

	semaphore = make(chan struct{}, config.Concurrency.MaxConversions)
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

	imageURL := buildImageURL(upstreamScheme, allowedHost, r.URL.Path, r.URL.RawQuery)

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
