package main

import (
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	WebP struct {
		Quality      int  `mapstructure:"quality"`
		Lossless     bool `mapstructure:"lossless"`
		NearLossless int  `mapstructure:"near_lossless"`
	} `mapstructure:"webp"`
	Cache struct {
		CacheEnabled       bool   `mapstructure:"cache_enabled"`
		NoCacheHeader      string `mapstructure:"nocache_header"`
		MaxBytes           int64  `mapstructure:"max_bytes"`
		NegativeTTLSeconds int    `mapstructure:"negative_ttl_seconds"`
		LruCache           int    `mapstructure:"lru_cache"` // deprecated, ignored
	} `mapstructure:"cache"`
	Concurrency struct {
		MaxGoroutines  int `mapstructure:"max_goroutines"` // deprecated alias of max_conversions
		MaxConversions int `mapstructure:"max_conversions"`
		MaxFetches     int `mapstructure:"max_fetches"`
	} `mapstructure:"concurrency"`
	HTTPClient struct {
		TimeoutSeconds        int `mapstructure:"timeout_seconds"`
		DialTimeoutSeconds    int `mapstructure:"dial_timeout_seconds"`
		KeepAlive             int `mapstructure:"keep_alive"`
		TLSHandshakeTimeout   int `mapstructure:"TLS_handshake_timeout"`
		ResponseHeaderTimeout int `mapstructure:"response_header_timeout"`
		ExpectContinueTimeout int `mapstructure:"expect_continue_timeout"`
		IdleConnTimeout       int `mapstructure:"idle_conn_timeout"`
		MaxIdleConnsPerHost   int `mapstructure:"max_idle_conns_per_host"`
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
		BindIP                   string `mapstructure:"bind_ip"`
		Port                     int    `mapstructure:"port"`
		ReadHeaderTimeoutSeconds int    `mapstructure:"read_header_timeout_seconds"`
		IdleTimeoutSeconds       int    `mapstructure:"idle_timeout_seconds"`
		CacheControl             string `mapstructure:"cache_control"`
	} `mapstructure:"http_server"`
	Log struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`
}

// config is the process-wide configuration, filled by loadConfig.
var config Config

// loadConfig reads config.yaml from dir into the global config, applies
// defaults and validates the result.
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
	config.applyDefaults()
	return config.validate()
}

func setDefaultInt(p *int, def int) {
	if *p <= 0 {
		*p = def
	}
}

func setDefaultInt64(p *int64, def int64) {
	if *p <= 0 {
		*p = def
	}
}

func setDefaultString(p *string, def string) {
	if *p == "" {
		*p = def
	}
}

// applyDefaults fills zero values so an older config.yaml keeps working.
func (c *Config) applyDefaults() {
	setDefaultInt(&c.WebP.Quality, 75)

	setDefaultString(&c.Cache.NoCacheHeader, "X-No-Cache")
	setDefaultInt64(&c.Cache.MaxBytes, 1<<30)
	if c.Cache.NegativeTTLSeconds == 0 {
		c.Cache.NegativeTTLSeconds = 30
	}

	if c.Concurrency.MaxConversions <= 0 && c.Concurrency.MaxGoroutines > 0 {
		c.Concurrency.MaxConversions = c.Concurrency.MaxGoroutines
	}
	setDefaultInt(&c.Concurrency.MaxConversions, runtime.NumCPU())
	setDefaultInt(&c.Concurrency.MaxFetches, 512)

	setDefaultInt(&c.HTTPClient.TimeoutSeconds, 30)
	setDefaultInt(&c.HTTPClient.DialTimeoutSeconds, 5)
	setDefaultInt(&c.HTTPClient.KeepAlive, 30)
	setDefaultInt(&c.HTTPClient.TLSHandshakeTimeout, 10)
	setDefaultInt(&c.HTTPClient.ResponseHeaderTimeout, 30)
	setDefaultInt(&c.HTTPClient.ExpectContinueTimeout, 1)
	setDefaultInt(&c.HTTPClient.IdleConnTimeout, 90)
	setDefaultInt(&c.HTTPClient.MaxIdleConnsPerHost, 256)

	setDefaultInt64(&c.Limits.MaxImageBytes, 20<<20)

	setDefaultString(&c.HTTPServer.BindIP, "127.0.0.1")
	setDefaultInt(&c.HTTPServer.Port, 8080)
	setDefaultInt(&c.HTTPServer.ReadHeaderTimeoutSeconds, 5)
	setDefaultInt(&c.HTTPServer.IdleTimeoutSeconds, 60)
	setDefaultString(&c.HTTPServer.CacheControl, "public, max-age=31536000, immutable")

	setDefaultString(&c.Exporter.BindIP, "127.0.0.1")
	setDefaultInt(&c.Exporter.Port, 9080)

	setDefaultString(&c.Log.Level, "info")
}

func (c *Config) validate() error {
	if len(c.AllowedHosts) == 0 {
		return fmt.Errorf("allowed_hosts must contain at least one host mapping")
	}
	if c.WebP.Quality < 1 || c.WebP.Quality > 100 {
		return fmt.Errorf("webp.quality must be in 1..100, got %d", c.WebP.Quality)
	}
	return nil
}

// NegativeTTL is how long origin 404s are remembered. The YAML default is
// 30 s; a negative value disables it (0 is indistinguishable from "unset").
func (c *Config) NegativeTTL() time.Duration {
	if c.Cache.NegativeTTLSeconds < 0 {
		return 0
	}
	return time.Duration(c.Cache.NegativeTTLSeconds) * time.Second
}

// newHTTPClient builds the upstream client. The service talks to a handful
// of origin hosts, so the idle pool per host is the main lever against
// connection churn.
func newHTTPClient(c *Config) *http.Client {
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   sec(c.HTTPClient.DialTimeoutSeconds),
				KeepAlive: sec(c.HTTPClient.KeepAlive),
			}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   sec(c.HTTPClient.TLSHandshakeTimeout),
			ResponseHeaderTimeout: sec(c.HTTPClient.ResponseHeaderTimeout),
			ExpectContinueTimeout: sec(c.HTTPClient.ExpectContinueTimeout),
			IdleConnTimeout:       sec(c.HTTPClient.IdleConnTimeout),
			MaxIdleConns:          c.HTTPClient.MaxIdleConnsPerHost,
			MaxIdleConnsPerHost:   c.HTTPClient.MaxIdleConnsPerHost,
		},
		Timeout: sec(c.HTTPClient.TimeoutSeconds),
	}
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
func buildImageURL(scheme, host, path, rawQuery string) string {
	u := scheme + host + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}
