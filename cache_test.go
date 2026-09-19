package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func newTestCache(t *testing.T, maxBytes int64, negTTL time.Duration) *ImageCache {
	t.Helper()
	c, err := NewImageCache(maxBytes, negTTL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestNewEntryComputesQuotedETag(t *testing.T) {
	e := NewEntry([]byte("hello"))
	if len(e.ETag) < 3 || e.ETag[0] != '"' || e.ETag[len(e.ETag)-1] != '"' {
		t.Fatalf("ETag must be a quoted string, got %q", e.ETag)
	}
	if e.ETag != NewEntry([]byte("hello")).ETag {
		t.Fatal("ETag must be deterministic")
	}
	if e.ETag == NewEntry([]byte("hellp")).ETag {
		t.Fatal("different data must give different ETag")
	}
}

func TestCacheSetThenGet(t *testing.T) {
	c := newTestCache(t, 1<<20, 0)
	c.Set("k", NewEntry([]byte("img")))
	c.Wait()
	got, ok := c.Get("k")
	if !ok || string(got.Data) != "img" {
		t.Fatalf("want hit with img, got ok=%v data=%q", ok, got.Data)
	}
}

func TestCacheMissReturnsFalse(t *testing.T) {
	c := newTestCache(t, 1<<20, 0)
	if _, ok := c.Get("absent"); ok {
		t.Fatal("want miss")
	}
}

func TestCacheEvictsWhenOverMaxBytes(t *testing.T) {
	const max = 64 << 10 // 64 KiB
	c := newTestCache(t, max, 0)
	blob := make([]byte, 8<<10) // 8 KiB each; 32 entries = 256 KiB inserted
	for i := 0; i < 32; i++ {
		c.Set(fmt.Sprintf("k%d", i), NewEntry(blob))
	}
	c.Wait()
	held := 0
	for i := 0; i < 32; i++ {
		if _, ok := c.Get(fmt.Sprintf("k%d", i)); ok {
			held++
		}
	}
	if held == 0 {
		t.Fatal("cache holds nothing after inserts")
	}
	if int64(held)*int64(len(blob)) > max {
		t.Fatalf("cache holds %d entries (%d bytes) above max %d", held, held*len(blob), max)
	}
}

func TestNegativeCacheRemembersStatusUntilTTL(t *testing.T) {
	c := newTestCache(t, 1<<20, 200*time.Millisecond)
	c.SetNegative("k", http.StatusNotFound)
	c.Wait()
	if code, ok := c.GetNegative("k"); !ok || code != http.StatusNotFound {
		t.Fatalf("want 404 negative hit, got ok=%v code=%d", ok, code)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := c.GetNegative("k"); ok {
		t.Fatal("negative entry should have expired")
	}
}

func TestNegativeCacheDisabledWhenTTLZero(t *testing.T) {
	c := newTestCache(t, 1<<20, 0)
	c.SetNegative("k", http.StatusNotFound)
	c.Wait()
	if _, ok := c.GetNegative("k"); ok {
		t.Fatal("negative cache must be disabled when TTL is 0")
	}
}

func TestCacheKeyIncludesQuality(t *testing.T) {
	if cacheKey("https://o/a.jpg", 60) == cacheKey("https://o/a.jpg", 80) {
		t.Fatal("keys for different qualities must differ")
	}
	if cacheKey("https://o/a.jpg", 60) != "https://o/a.jpg|q=60" {
		t.Fatalf("unexpected key format %q", cacheKey("https://o/a.jpg", 60))
	}
}
