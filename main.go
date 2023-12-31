package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
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
	Exporter     struct {
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

type ImageResult struct {
	Data  []byte
	Error error
}

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
	logger     *logrus.Logger
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

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	if err := viper.ReadInConfig(); err != nil {
		logrus.Fatalf("Error reading config file: %v", err)
	}

	if err := viper.Unmarshal(&config); err != nil {
		logrus.Fatalf("Unable to decode into struct: %v", err)
	}

	var err error
	imgCache, err = lru.New2Q[string, []byte](config.Cache.LruCache) // Size of the cache
	if err != nil {
		logrus.Fatalf("Failed to create ARCCache: %v", err)
	}

	logger = logrus.New()
	logger.Out = os.Stdout
	logger.Level = logrus.DebugLevel
	logger.Formatter = &logrus.JSONFormatter{}

	httpClient = &http.Client{
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout:   time.Duration(config.HTTPClient.DialTimeoutSeconds) * time.Second,
				KeepAlive: time.Duration(config.HTTPClient.KeepAlive) * time.Second,
			}).Dial,
			TLSHandshakeTimeout:   time.Duration(config.HTTPClient.TLSHandshakeTimeout) * time.Second,
			ResponseHeaderTimeout: time.Duration(config.HTTPClient.ResponseHeaderTimeout) * time.Second,
			ExpectContinueTimeout: time.Duration(config.HTTPClient.ExpectContinueTimeout) * time.Second,
			MaxConnsPerHost:       0,
			MaxIdleConnsPerHost:   0,
			MaxIdleConns:          100,
		},
		Timeout: time.Second * time.Duration(config.HTTPClient.TimeoutSeconds),
	}

	semaphore = make(chan struct{}, config.Concurrency.MaxGoroutines)
}

func main() {
	router := mux.NewRouter()
	metricsRouter := mux.NewRouter()
	router.HandleFunc("/{image:.*}", handleRequest)
	router.HandleFunc("/health", healthCheckHandler)
	metricsRouter.Handle("/metrics", prometheusHandler())

	srv := &http.Server{
		Addr:    fmt.Sprintf("%v:%v", config.HTTPServer.BindIP, config.HTTPServer.Port),
		Handler: router,
	}

	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	go func() {
		logger.Info(fmt.Sprintf("Server started at http://%v:%v", config.HTTPServer.BindIP, config.HTTPServer.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("ListenAndServe(): %v", err)
		}
	}()

	metricsSrv := &http.Server{
		Addr:    fmt.Sprintf("%v:%v", config.Exporter.BindIP, config.Exporter.Port),
		Handler: basicAuthMiddleware(metricsRouter),
	}

	go func() {
		logger.Info(fmt.Sprintf("Exporter started at http://%v:%v", config.Exporter.BindIP, config.Exporter.Port))
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("ListenAndServe(): %v", err)
		}
	}()

	gracefulShutdown(srv)
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	remoteHost := r.Host

	if r.URL.Path == "/metrics" {
		// Allow unrestricted access to the /metrics endpoint
		prometheusHandler().ServeHTTP(w, r)
		return
	}
	if r.URL.Path == "/health" {
		// Allow unrestricted access to the /health endpoint
		healthCheckHandler(w, r)
		return
	}

	if remoteHost == "" {
		http.Error(w, "Host header is missing", http.StatusBadRequest)
		return
	}

	statusCode := strconv.Itoa(http.StatusOK)
	duration := time.Since(startTime).Seconds()

	requestsTotal.WithLabelValues(statusCode, r.Method).Inc()
	responseDuration.WithLabelValues(statusCode, r.Method).Observe(duration)

	// Check if the remote host is allowed and get the corresponding imageURL host
	allowedHost, exists := config.AllowedHosts[remoteHost]
	if !exists {
		invalidHostsCount.Inc()
		logger.Warn("Unauthorized access attempt from host: ", remoteHost)
		http.Error(w, "Host not allowed", http.StatusForbidden)
		return
	}

	cacheCount := imgCache.Len()
	logger.Info("Cache element count: ", cacheCount)

	imageURL := "https://" + allowedHost + r.URL.Path //+ "?" + r.URL.RawQuery

	nocacheHeader := r.Header.Get(config.Cache.NoCacheHeader)
	if config.Cache.CacheEnabled && nocacheHeader != "true" {
		if cachedImage, found := imgCache.Get(imageURL); found {
			logger.Info("Cache hit for URL: ", imageURL)
			serveCachedImage(w, cachedImage)
			cacheHitsTotal.Inc()
			return
		}
		cacheMissesTotal.Inc()
	}

	logger.Info("Cache miss for URL: ", imageURL)

	semaphoreWaitStart := time.Now()
	logger.Info("Waiting for semaphore slot")
	semaphore <- struct{}{}
	defer func() { <-semaphore }()
	semaphoreWaitDuration := time.Since(semaphoreWaitStart)
	logger.WithFields(logrus.Fields{
		"semaphoreWaitDuration": semaphoreWaitDuration,
	}).Info("Semaphore slot acquired")

	resultChan := make(chan ImageResult)
	go processImageAsync(imageURL, resultChan, r)

	result := <-resultChan
	if result.Error != nil {
		logger.WithFields(logrus.Fields{"error": result.Error.Error(), "url": imageURL}).Error("Error processing image")
		http.Error(w, fmt.Sprintf("Error processing image: %v", result.Error), http.StatusInternalServerError)
		errorsTotal.Inc()
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Content-Length", strconv.Itoa(len(result.Data)))
	w.Write(result.Data)
}

func processImageAsync(imageURL string, resultChan chan ImageResult, req *http.Request) {

	var quality int

	// Check if x-webp-quality header is provided in the request
	if qualityHeader := req.Header.Get("x-webp-quality"); qualityHeader != "" {
		if q, err := strconv.Atoi(qualityHeader); err == nil {
			quality = q
		}
	}

	// Use default quality from config if x-webp-quality header is not provided or invalid
	if quality == 0 {
		quality = config.WebP.Quality
	}

	resp, err := httpClient.Get(imageURL)
	if err != nil {
		resultChan <- ImageResult{Error: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		resultChan <- ImageResult{Error: fmt.Errorf("HTTP error from remote host: %s", resp.Status)}
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if !isSupportedImageFormat(contentType) {
		resultChan <- ImageResult{Error: fmt.Errorf("Unsupported image format")}
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		resultChan <- ImageResult{Error: fmt.Errorf("Error reading image body: %v", err)}
		return
	}
	// Calculate the size of the original image
	totalImageSizeBeforeConversion.Add(float64(len(body)))

	options := bimg.Options{
		Quality:  quality,
		Lossless: config.WebP.Lossless,
		Type:     bimg.WEBP,
	}

	newImage, err := bimg.NewImage(body).Process(options)
	totalImageSizeAfterConversion.Add(float64(len(newImage)))
	if err != nil {
		resultChan <- ImageResult{Error: fmt.Errorf("Error converting image: %v", err)}
		return
	}

	imgCache.Add(imageURL, newImage)

	resultChan <- ImageResult{Data: newImage}
}

func serveCachedImage(w http.ResponseWriter, cachedImageData interface{}) {
	if imageData, ok := cachedImageData.([]byte); ok {
		w.Header().Set("Content-Type", "image/webp")
		w.Header().Set("Content-Length", strconv.Itoa(len(imageData)))
		w.Write(imageData)
	} else {
		logger.Error("Invalid data type found in cache")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func isSupportedImageFormat(contentType string) bool {
	supportedFormats := []string{"jpeg", "jpg", "png", "gif", "bmp"}
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

func gracefulShutdown(srv *http.Server) {
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	<-stopChan
	logger.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatalf("Server forced to shutdown: %v", err)
	}

	logger.Info("Server gracefully stopped")
}

func isAllowedHost(host string) bool {
	for _, allowedHost := range config.AllowedHosts {
		if host == allowedHost {
			return true
		}
	}
	return false
}

func prometheusHandler() http.Handler {
	return promhttp.Handler()
}

func basicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := config.Exporter.User
		password := config.Exporter.Password

		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
