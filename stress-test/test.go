package main

import (
	"fmt"
	"net/http"
	"sync"
)

const (
	baseURL       = "http://localhost:8080/" // Change this to your server's URL
	testImagePath = "image_path"             // Replace with a valid image path
	numRequests   = 10000                    // Total number of requests to send
	concurrency   = 150                      // Number of concurrent requests
	hostHeader    = "cdn.example.com"        // Replace with a host from allowed_hosts
)

func main() {
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency) // Semaphore for limiting concurrency

	for i := 0; i < numRequests; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Append a unique query parameter to bypass the cache
			imageURL := fmt.Sprintf("%s%s?nocache=%d", baseURL, testImagePath, i)
			// Create a new HTTP request
			req, err := http.NewRequest("GET", imageURL, nil)
			if err != nil {
				fmt.Printf("Request %d failed to create: %s\n", i, err)
				<-sem
				return
			}

			// Set the Host header to match the expected host from the whitelist
			req.Host = hostHeader

			// Send the request
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Printf("Request %d failed: %s\n", i, err)
				<-sem
				return
			}
			defer resp.Body.Close()
			fmt.Printf("Request %d completed with status code: %d\n", i, resp.StatusCode)
			<-sem
		}(i)
	}

	wg.Wait()
	fmt.Println("Stress test completed.")
}
