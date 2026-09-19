package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngServer(t *testing.T) *httptest.Server {
	t.Helper()
	data := pngBytes(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchReturnsBodyAndContentType(t *testing.T) {
	srv := pngServer(t)
	f := NewFetcher(srv.Client(), 1<<20, 4)
	body, err := f.Fetch(context.Background(), srv.URL+"/a.png")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Release()
	if body.ContentType != "image/png" {
		t.Fatalf("want image/png, got %q", body.ContentType)
	}
	if !bytes.Equal(body.Bytes(), pngBytes(t)) {
		t.Fatal("body differs from served bytes")
	}
}

func TestFetchReportsUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), 1<<20, 4)
	_, err := f.Fetch(context.Background(), srv.URL+"/missing.png")
	var us *UpstreamStatusError
	if !errors.As(err, &us) || us.Code != http.StatusNotFound {
		t.Fatalf("want UpstreamStatusError 404, got %v", err)
	}
}

func TestFetchRejectsUnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>"))
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), 1<<20, 4)
	if _, err := f.Fetch(context.Background(), srv.URL+"/x"); err == nil {
		t.Fatal("want error for text/html, got nil")
	}
}

func TestFetchRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), 512, 4)
	if _, err := f.Fetch(context.Background(), srv.URL+"/big.png"); err == nil {
		t.Fatal("want error for oversized body, got nil")
	}
}

func TestFetchRejectsOversizedContentLengthBeforeReading(t *testing.T) {
	var read atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "4096")
		read.Add(1)
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), 512, 4)
	_, err := f.Fetch(context.Background(), srv.URL+"/big.png")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	// The handler ran once (the response headers were needed) but the error
	// must come from the Content-Length check, not from reading 4 KiB.
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("want ErrImageTooLarge, got %v", err)
	}
}

func TestFetchLimitsConcurrentRequests(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes(t))
	}))
	defer srv.Close()

	f := NewFetcher(srv.Client(), 1<<20, 2)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := f.Fetch(context.Background(), srv.URL+"/a.png"); err == nil {
				b.Release()
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("want at most 2 concurrent fetches, saw %d", peak.Load())
	}
}

func TestFetchHonoursContextWhileWaitingForSlot(t *testing.T) {
	f := NewFetcher(http.DefaultClient, 1<<20, 1)
	f.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.Fetch(ctx, "http://127.0.0.1:1/x.png"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestShouldPoolRejectsOversizedBuffers(t *testing.T) {
	small := bytes.NewBuffer(make([]byte, 0, 1024))
	large := bytes.NewBuffer(make([]byte, 0, maxPooledBuffer+1))
	if !shouldPool(small) {
		t.Fatal("1 KiB buffer should be pooled")
	}
	if shouldPool(large) {
		t.Fatal("buffer above maxPooledBuffer must not be pooled")
	}
}

func TestIsSupportedImageFormat(t *testing.T) {
	for ct, want := range map[string]bool{
		"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
		"image/png; charset=binary": true, "text/html": false, "": false,
	} {
		if got := isSupportedImageFormat(ct); got != want {
			t.Errorf("%q: want %v, got %v", ct, want, got)
		}
	}
}
