// Command origin is a synthetic image origin for load testing ngAX.
// It serves /img/<n>.png with content that depends on n and exposes the
// number of requests served at /count.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	count atomic.Int64
	cache sync.Map // n -> []byte
)

func imageFor(n int, size int) []byte {
	if v, ok := cache.Load(n); ok {
		return v.([]byte)
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i+0] = byte(x*n) ^ byte(y)
			img.Pix[i+1] = byte(y*n) ^ byte(x)
			img.Pix[i+2] = byte((x + y) * n)
			img.Pix[i+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Fatal(err)
	}
	cache.Store(n, buf.Bytes())
	return buf.Bytes()
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address")
	size := flag.Int("size", 512, "image side in pixels")
	flag.Parse()

	http.HandleFunc("/count", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, count.Load())
	})
	http.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		count.Store(0)
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/img/"), ".png")
		n, err := strconv.Atoi(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(imageFor(n, *size))
	})
	log.Printf("origin listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
