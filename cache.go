package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/dgraph-io/ristretto/v2"
)

// Entry is a converted image ready to serve.
type Entry struct {
	Data []byte
	ETag string
}

// NewEntry wraps converted bytes and computes their ETag once.
func NewEntry(data []byte) Entry {
	return Entry{
		Data: data,
		ETag: `"` + strconv.FormatUint(xxhash.Sum64(data), 16) + `"`,
	}
}

func cacheKey(imageURL string, quality int) string {
	return imageURL + "|q=" + strconv.Itoa(quality)
}

// ImageCache holds converted images bounded by total bytes, plus a small
// TTL cache of upstream status codes for images that do not exist.
type ImageCache struct {
	images      *ristretto.Cache[string, Entry]
	negative    *ristretto.Cache[string, int]
	negativeTTL time.Duration
}

// NewImageCache creates a cache holding at most maxBytes of image data.
// negativeTTL <= 0 disables the negative cache.
func NewImageCache(maxBytes int64, negativeTTL time.Duration) (*ImageCache, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("cache max bytes must be > 0, got %d", maxBytes)
	}
	// NumCounters should be ~10x the expected number of items; assume a
	// 32 KiB average image and clamp to a sane range.
	counters := maxBytes / (32 << 10) * 10
	if counters < 1_000 {
		counters = 1_000
	}
	if counters > 100_000_000 {
		counters = 100_000_000
	}
	images, err := ristretto.NewCache(&ristretto.Config[string, Entry]{
		NumCounters: counters,
		MaxCost:     maxBytes,
		BufferItems: 64,
		Metrics:     true,
		// Without this, ristretto adds its own per-item storage overhead
		// (sizeof its internal storeItem struct) to each item's cost, so
		// CostAdded/CostEvicted would no longer equal the exact byte costs
		// passed to Set. We want Bytes() to report exactly what callers
		// pass in.
		IgnoreInternalCost: true,
	})
	if err != nil {
		return nil, fmt.Errorf("creating image cache: %w", err)
	}
	c := &ImageCache{images: images, negativeTTL: negativeTTL}
	if negativeTTL > 0 {
		c.negative, err = ristretto.NewCache(&ristretto.Config[string, int]{
			NumCounters: 100_000,
			MaxCost:     10_000, // number of remembered misses
			BufferItems: 64,
		})
		if err != nil {
			images.Close()
			return nil, fmt.Errorf("creating negative cache: %w", err)
		}
	}
	return c, nil
}

func (c *ImageCache) Get(key string) (Entry, bool) {
	return c.images.Get(key)
}

// Set stores e. The insert is asynchronous and may be rejected by the
// admission policy, which is acceptable for a cache.
func (c *ImageCache) Set(key string, e Entry) {
	c.images.Set(key, e, int64(len(e.Data)))
}

// Bytes returns the approximate bytes of admitted entries: ristretto counts
// cost only for items the admission policy accepted and subtracts it on eviction.
func (c *ImageCache) Bytes() int64 {
	m := c.images.Metrics
	return int64(m.CostAdded()) - int64(m.CostEvicted())
}

func (c *ImageCache) GetNegative(key string) (int, bool) {
	if c.negative == nil {
		return 0, false
	}
	return c.negative.Get(key)
}

func (c *ImageCache) SetNegative(key string, status int) {
	if c.negative == nil {
		return
	}
	c.negative.SetWithTTL(key, status, 1, c.negativeTTL)
}

// Wait blocks until pending sets are applied. Intended for tests.
func (c *ImageCache) Wait() {
	c.images.Wait()
	if c.negative != nil {
		c.negative.Wait()
	}
}

func (c *ImageCache) Close() {
	c.images.Close()
	if c.negative != nil {
		c.negative.Close()
	}
}
