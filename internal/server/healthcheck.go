package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// HealthCheck GETs url and returns (0, detail) if it responds 2xx, (1, detail)
// otherwise. detail is a short human reason — "HTTP <code>" for a response, or
// the transport error for a failed connection. It backs the container
// HEALTHCHECK (the distroless image ships no curl) and the `yore healthcheck`
// CLI, which prints detail so an interactive probe isn't silent.
func HealthCheck(url string) (int, string) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		// Drop the *url.Error's `Get "url":` prefix; the caller already prints
		// the url, so the bare transport reason reads cleaner.
		reason := err.Error()
		if inner := errors.Unwrap(err); inner != nil {
			reason = inner.Error()
		}
		return 1, reason
	}
	defer resp.Body.Close()
	detail := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0, detail
	}
	return 1, detail
}
