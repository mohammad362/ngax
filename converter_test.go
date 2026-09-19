package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func isWebP(b []byte) bool {
	return len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
}

func TestToWebPConvertsPNG(t *testing.T) {
	c := NewConverter(2, false)
	out, err := c.ToWebP(context.Background(), pngBytes(t), 75)
	if err != nil {
		t.Fatal(err)
	}
	if !isWebP(out) {
		t.Fatalf("output is not WebP: %q", out[:min(16, len(out))])
	}
}

func TestToWebPQualityChangesOutput(t *testing.T) {
	c := NewConverter(2, false)
	src := pngBytes(t)
	lo, err := c.ToWebP(context.Background(), src, 10)
	if err != nil {
		t.Fatal(err)
	}
	hi, err := c.ToWebP(context.Background(), src, 100)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(lo, hi) {
		t.Fatal("quality 10 and 100 produced identical output")
	}
}

func TestToWebPRejectsGarbage(t *testing.T) {
	c := NewConverter(1, false)
	if _, err := c.ToWebP(context.Background(), []byte("not an image"), 75); err == nil {
		t.Fatal("want error for garbage input")
	}
}

func TestToWebPHonoursContextWhileWaitingForWorker(t *testing.T) {
	c := NewConverter(1, false)
	c.sem <- struct{}{} // occupy the only worker
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.ToWebP(ctx, pngBytes(t), 75)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestToWebPLimitsConcurrency(t *testing.T) {
	c := NewConverter(2, false)
	var inFlight, peak atomic.Int32
	src := pngBytes(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.sem <- struct{}{}
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			<-c.sem
		}()
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("semaphore admitted %d workers, want <= 2", peak.Load())
	}
	if _, err := c.ToWebP(context.Background(), src, 75); err != nil {
		t.Fatal(err)
	}
}
