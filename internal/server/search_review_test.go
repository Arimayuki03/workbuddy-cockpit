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
// 200 chars), so the channel-check verdict must rely on the stable
// Error.ChannelCheck flag captured from the full body. The review fix also makes
// this a request-level 400 that stops rotation immediately (no account penalty,
// no wasted healthy accounts).
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
	up, state := newSearchTestUpstream(t, search200Body(late))
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "test-key"})
	h.MountSearch()

	rec := postSearchWithKey(h, "test-key", searchTestBody("golang"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (request-level channel check) body=%s", rec.Code, rec.Body)
	}
	if calls, _ := state.counts(); calls != 1 {
		t.Fatalf("channel check must stop rotation after 1 upstream call, got %d", calls)
	}
	for _, uid := range []string{"u1", "u2"} {
		st := searchStatusOf(t, p, uid)
		if st.Disabled {
			t.Fatalf("%s: late channel-check marker must not hard-disable: %+v", uid, st)
		}
		if st.Cooling || st.ConsecutiveFails != 0 {
			t.Fatalf("%s: channel check must be request-scoped (no cooldown/fail count), got %+v", uid, st)
		}
	}
}
