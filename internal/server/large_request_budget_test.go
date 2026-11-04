package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

func TestLargeRequestBudgetDefaults(t *testing.T) {
	h := NewHandler(Config{})
	if h.cfg.LargeRequestBytes != 2<<20 {
		t.Fatalf("threshold=%d want %d", h.cfg.LargeRequestBytes, 2<<20)
	}
	if h.cfg.LargeRequestBudget != 6 || h.cfg.LargeRequestBudgetWindow != 120*time.Second {
		t.Fatalf("budget=%d window=%s", h.cfg.LargeRequestBudget, h.cfg.LargeRequestBudgetWindow)
	}
	if h.cfg.LargeRequestCooldown != 0 {
		t.Fatalf("legacy cooldown=%s want 0", h.cfg.LargeRequestCooldown)
	}
}

func TestLargeRequestBudgetExhaustionAndWindow(t *testing.T) {
	h := NewHandler(Config{LargeRequestBytes: 1, LargeRequestBudget: 2, LargeRequestBudgetWindow: 10 * time.Second})
	now := time.Now()
	h.noteLargeUse("u1", now)
	if h.largeExclusions(now)["u1"] {
		t.Fatal("1/2 条不应排除")
	}
	h.noteLargeUse("u1", now)
	if !h.largeExclusions(now)["u1"] {
		t.Fatal("用满预算应排除")
	}
	if h.largeExclusions(now.Add(11 * time.Second))["u1"] {
		t.Fatal("窗口滑过后应恢复")
	}
}

func TestLargeRequestLegacyRotation(t *testing.T) {
	h := NewHandler(Config{
		LargeRequestBytes:    1,
		LargeRequestBudget:   -1,
		LargeRequestCooldown: 30 * time.Second,
	})
	now := time.Now()
	h.noteLargeUse("u1", now)
	if !h.largeExclusions(now)["u1"] {
		t.Fatal("legacy 轮换窗口内应排除")
	}
}

func TestLargeRequestBudgetMigratesStickySession(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"a", "b"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
	)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})
	h := NewHandler(Config{
		Pool:                     p,
		Upstream:                 up,
		Session:                  sess,
		MaxRotate:                2,
		LargeRequestBytes:        1,
		LargeRequestBudget:       1,
		LargeRequestBudgetWindow: time.Minute,
	})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-budget"}}`
	send := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	send()
	first, ok := st.lastUID("conv-budget")
	if !ok || first == "" {
		t.Fatal("首条大请求应绑定会话")
	}
	send()
	second, _ := st.lastUID("conv-budget")
	if second == "" || second == first {
		t.Fatalf("预算用满后应迁移会话: first=%s second=%s", first, second)
	}
}
