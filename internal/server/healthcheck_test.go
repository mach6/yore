package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHealthCheck covers the three outcomes: a 2xx response is healthy with an
// "HTTP <code>" detail, a non-2xx response is unhealthy with the same detail
// shape, and an unreachable address is unhealthy with a non-empty transport
// reason.
func TestHealthCheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ok.Close)

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)

	tests := []struct {
		name       string
		url        string
		wantCode   int
		wantDetail string // "" => assert only that detail is non-empty
	}{
		{"2xx is healthy", ok.URL, 0, "HTTP 200"},
		{"503 is unhealthy", down.URL, 1, "HTTP 503"},
		// Nothing listens on port 1: a refused connection is a transport error.
		{"unreachable is unhealthy", "http://127.0.0.1:1/v1/health", 1, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, detail := HealthCheck(tc.url)
			require.Equal(t, tc.wantCode, code)
			if tc.wantDetail == "" {
				require.NotEmpty(t, detail)
			} else {
				require.Equal(t, tc.wantDetail, detail)
			}
		})
	}
}
