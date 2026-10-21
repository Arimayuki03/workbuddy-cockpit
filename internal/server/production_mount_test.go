package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// Production main calls MountResponses, MountAnthropic and MountSearch together
// after NewHandler. This regression mounts all three in the same order and
// proves /v1/search/messages is live (not a 404) with normal auth.
func TestProductionMountsIncludeSearch(t *testing.T) {
	up, st := newSearchTestUpstream(t, nil)
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "test-key"})
	h.MountResponses()
	h.MountAnthropic()
	h.MountSearch()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(searchTestBody("golang")))
	req.Header.Set("Authorization", "Bearer test-key")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("search route not mounted in production assembly: code=%d body=%s", rec.Code, rec.Body)
	}
	if calls, _ := st.counts(); calls != 1 {
		t.Errorf("upstream search calls=%d want 1", calls)
	}
}
