package server

import (
	"net/http"
	"time"
)

// HealthCheck GETs url and returns 0 if it responds 2xx, 1 otherwise. It backs
// the container HEALTHCHECK, since the distroless image ships no curl.
func HealthCheck(url string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0
	}
	return 1
}
