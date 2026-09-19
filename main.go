package main

import (
	"github.com/sirupsen/logrus"
)

func main() {
	if err := loadConfig("."); err != nil {
		logrus.Fatal(err)
	}
	log := newLogger(config.Log.Level)
	if config.Cache.LruCache != 0 {
		log.Warn("cache.lru_cache is deprecated and ignored; use cache.max_bytes")
	}

	cache, err := NewImageCache(config.Cache.MaxBytes, config.NegativeTTL())
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()
	registerCacheBytes(cache)

	fetcher := NewFetcher(newHTTPClient(&config), config.Limits.MaxImageBytes, config.Concurrency.MaxFetches)
	conv := NewConverter(config.Concurrency.MaxConversions, config.WebP.Lossless)
	handler := NewHandler(&config, cache, fetcher, conv, log)

	log.WithFields(logrus.Fields{
		"conversion_workers": conv.Workers(),
		"max_fetches":        config.Concurrency.MaxFetches,
		"cache_max_bytes":    config.Cache.MaxBytes,
		"hosts":              len(config.AllowedHosts),
	}).Info("ngax starting")

	runServers(log,
		newPublicServer(&config, newRouter(handler)),
		newMetricsServer(&config),
		newPprofServer(),
	)
}
