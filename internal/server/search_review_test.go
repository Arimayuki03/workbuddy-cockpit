package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// postSearchWithKey mounts the search route and sends an authenticated request.
func postSearchWithKey(h *Handler, key, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	h.ServeHTTP(rec, req)
	return rec
}

func search200Body(body string) func(http.ResponseWriter, *http.Request, *searchUpstreamState) {
	return func(w http.ResponseWriter, _ *http.Request, _ *searchUpstreamState) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// HTTP 200 + business rate-limit code must still take the account error policy:
// cool the account and return 429, not a request-level 400 that leaves the
// throttled account healthy and selectable for the next search.
func TestSearchBusinessSoftRateOnHTTP200CoolsAccount(t *testing.T) {
	up, _ := newSearchTestUpstream(t, search200Body(`{"code":6004,"msg":"rate limit exceeded"}`))
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "test-key"})
	h.MountSearch()

	rec := postSearchWithKey(h, "test-key", searchTestBody("golang"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	st := searchStatusOf(t, p, "u1")
	if !st.Cooling || st.Disabled {
		t.Fatalf("business soft-rate must soft-cool, not disable: %+v", st)
	}
}

// HTTP 200 + 12153 is a dead session: it must disable the account (same as the
// HTTP >= 400 path), not be returned as a generic request error.
func TestSearchBusinessSessionDeadOnHTTP200Disables(t *testing.T) {
	up, _ := newSearchTestUpstream(t, search200Body(`{"code":12153,"msg":"Offline user session not found"}`))
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "test-key"})
	h.MountSearch()

	rec := postSearchWithKey(h, "test-key", searchTestBody("golang"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 body=%s", rec.Code, rec.Body)
	}
	st := searchStatusOf(t, p, "u1")
	if !st.Disabled {
		t.Fatalf("12153 business error must disable the account: %+v", st)
	}
}

// The channel-check marker may sit after the first 200 bytes while
// `request illegal` appears early. Search errors carry only ue.Msg (truncated to
// 200 chars), so applyErrorPolicy must rely on the stable Error.ChannelCheck
// flag captured from the full body — otherwise it would hard-disable a healthy
// account even though the upstream only rejected the channel.
func TestSearchChannelCheckLateMarkerDoesNotDisable(t *testing.T) {
	late := `{"code":"11-128","msg":"request illegal","displayTips":{"en":"` +
		strings.Repeat("x", 300) +
		` This call failed channel verification, possibly because it was not sent from an official client."}}`
	if !upstream.IsChannelCheck(late) {
		t.Fatal("fixture is not a channel check")
	}
	if upstream.IsChannelCheck(late[:200]) {
		t.Fatal("fixture must keep the channel marker beyond the 200-byte truncation point")
	}
	up, _ := newSearchTestUpstream(t, search200Body(late))
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "test-key"})
	h.MountSearch()

	rec := postSearchWithKey(h, "test-key", searchTestBody("golang"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403 (account fault after rotation) body=%s", rec.Code, rec.Body)
	}
	st := searchStatusOf(t, p, "u1")
	if st.Disabled {
		t.Fatalf("late channel-check marker must not hard-disable: %+v", st)
	}
	if !st.Cooling || !strings.Contains(st.Reason, "channel check") {
		t.Fatalf("channel check must be a soft cooldown, got %+v", st)
	}
}
