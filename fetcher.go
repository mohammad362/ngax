package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// ErrImageTooLarge is returned when the origin body exceeds the configured limit.
var ErrImageTooLarge = errors.New("image exceeds size limit")

// UpstreamStatusError reports a non-200 response from the origin.
type UpstreamStatusError struct {
	Code int
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned HTTP %d", e.Code)
}

// maxPooledBuffer keeps very large buffers out of the pool so a few huge
// images do not pin memory forever.
const maxPooledBuffer = 4 << 20

// Body is a fetched origin response. Call Release when the bytes are no
// longer needed so the buffer returns to the pool.
type Body struct {
	ContentType string
	buf         *bytes.Buffer
	pool        *sync.Pool
}

func (b *Body) Bytes() []byte { return b.buf.Bytes() }

func (b *Body) Release() {
	if b.buf == nil {
		return
	}
	if b.buf.Cap() <= maxPooledBuffer {
		b.buf.Reset()
		b.pool.Put(b.buf)
	}
	b.buf = nil
}

// Fetcher downloads origin images with a concurrency cap and a size limit.
type Fetcher struct {
	client   *http.Client
	maxBytes int64
	sem      chan struct{}
	pool     sync.Pool
}

func NewFetcher(client *http.Client, maxBytes int64, maxInFlight int) *Fetcher {
	if maxInFlight <= 0 {
		maxInFlight = 1
	}
	return &Fetcher{
		client:   client,
		maxBytes: maxBytes,
		sem:      make(chan struct{}, maxInFlight),
		pool:     sync.Pool{New: func() any { return new(bytes.Buffer) }},
	}
}

// Fetch downloads url. It blocks while all fetch slots are busy, honouring ctx.
func (f *Fetcher) Fetch(ctx context.Context, url string) (*Body, error) {
	select {
	case f.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-f.sem }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamStatusError{Code: resp.StatusCode}
	}
	contentType := resp.Header.Get("Content-Type")
	if !isSupportedImageFormat(contentType) {
		return nil, fmt.Errorf("unsupported image format %q", contentType)
	}
	if resp.ContentLength > f.maxBytes {
		return nil, fmt.Errorf("%w: content-length %d > %d", ErrImageTooLarge, resp.ContentLength, f.maxBytes)
	}

	buf := f.pool.Get().(*bytes.Buffer)
	buf.Reset()
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	}
	n, err := io.Copy(buf, io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		buf.Reset()
		f.pool.Put(buf)
		return nil, fmt.Errorf("reading image body: %w", err)
	}
	if n > f.maxBytes {
		buf.Reset()
		f.pool.Put(buf)
		return nil, fmt.Errorf("%w: body > %d bytes", ErrImageTooLarge, f.maxBytes)
	}
	totalImageSizeBeforeConversion.Add(float64(n))
	return &Body{ContentType: contentType, buf: buf, pool: &f.pool}, nil
}

func isSupportedImageFormat(contentType string) bool {
	for _, format := range []string{"jpeg", "jpg", "png", "gif", "bmp", "webp", "tiff"} {
		if strings.Contains(contentType, format) {
			return true
		}
	}
	return false
}
