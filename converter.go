package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/h2non/bimg"
)

var initVips sync.Once

// Converter turns source images into WebP with at most `workers` conversions
// running at once. Parallelism comes from concurrent requests, so libvips
// itself is pinned to one thread per operation.
type Converter struct {
	sem      chan struct{}
	lossless bool
	// convert performs the actual encode; replaced in tests to observe concurrency.
	convert func(src []byte, opts bimg.Options) ([]byte, error)
}

func NewConverter(workers int, lossless bool) *Converter {
	if workers <= 0 {
		workers = 1
	}
	initVips.Do(func() {
		if os.Getenv("VIPS_CONCURRENCY") == "" {
			os.Setenv("VIPS_CONCURRENCY", "1")
		}
		bimg.Initialize()
		// A proxy sees mostly unique inputs; the vips operation cache would
		// only pin memory.
		bimg.VipsCacheSetMax(0)
		bimg.VipsCacheSetMaxMem(0)
	})
	return &Converter{
		sem:      make(chan struct{}, workers),
		lossless: lossless,
		convert: func(src []byte, opts bimg.Options) ([]byte, error) {
			return bimg.NewImage(src).Process(opts)
		},
	}
}

func (c *Converter) Workers() int { return cap(c.sem) }

// ToWebP converts src. It waits for a free worker, honouring ctx.
func (c *Converter) ToWebP(ctx context.Context, src []byte, quality int) ([]byte, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()

	out, err := c.convert(src, bimg.Options{
		Quality:  quality,
		Lossless: c.lossless,
		Type:     bimg.WEBP,
	})
	if err != nil {
		return nil, fmt.Errorf("converting image: %w", err)
	}
	totalImageSizeAfterConversion.Add(float64(len(out)))
	return out, nil
}
