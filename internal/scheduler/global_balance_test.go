package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// recordingCreditSink captures the read-only global balance observation without
// writing files; it also proves the global path does not emit a checkin record.
type recordingCreditSink struct {
	mu            sync.Mutex
	balanceUIDs   []string
	balanceAmount []int64
	checkinCalls  int
}

func (s *recordingCreditSink) RecordBalance(uid, nickname string, credits int64, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balanceUIDs = append(s.balanceUIDs, uid)
	s.balanceAmount = append(s.balanceAmount, credits)
	return 1
}

func (s *recordingCreditSink) RecordCheckin(uid, nickname string, before, after int64, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkinCalls++
	return 1
}

// A stale global token must be refreshed before the read-only balance call;
// otherwise the balance endpoint returns 401 and the scheduled checkin records
// a failure for an otherwise healthy account. The refreshed token must also be
// persisted through SaveAtomic.
func TestGlobalCheckinRefreshesExpiredTokenThenReadsBalance(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var mu sync.Mutex
	var refreshCalls, balanceCalls int
	var balanceAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			refreshCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`)
		case "/billing/meter/get-user-resource":
			balanceCalls++
			balanceAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, globalResourceBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-g1.json")
	a := &auth.Auth{UID: "g1", AccessToken: "old-at", RefreshToken: "old-rt",
		ExpiresAt: 1, Domain: "www.workbuddy.ai", FilePath: path}
	p := pool.New("")
	p.Add(a)
	s := New(Config{Pool: p, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, ChatBaseGlobal: srv.URL,
		BillingBaseCN: srv.URL, BillingBaseGlobal: srv.URL, GlobalEnabled: true,
	}})
	sink := &recordingCreditSink{}
	s.SetCreditSink(sink)

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshCalls != 1 {
		t.Errorf("refresh calls=%d want 1", refreshCalls)
	}
	if balanceCalls != 1 {
		t.Errorf("balance calls=%d want 1", balanceCalls)
	}
	if balanceAuth != "Bearer new-at" {
		t.Errorf("balance Authorization=%q want refreshed token", balanceAuth)
	}
	if len(out) != 1 || out[0].Status != CheckinSkipped || out[0].Credits == nil || *out[0].Credits != 100 {
		t.Fatalf("outcome=%+v want skipped/global credits=100", out)
	}
	if a.AccessTokenValue() != "new-at" || a.RefreshTokenValue() != "new-rt" {
		t.Errorf("token not updated: at=%q rt=%q", a.AccessTokenValue(), a.RefreshTokenValue())
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read persisted auth: %v", rerr)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	authDoc, _ := doc["auth"].(map[string]any)
	if authDoc == nil || authDoc["accessToken"] != "new-at" {
		t.Errorf("refreshed token not persisted: %v", doc["auth"])
	}
	if len(sink.balanceUIDs) != 1 || sink.balanceUIDs[0] != "g1" || sink.balanceAmount[0] != 100 {
		t.Errorf("credit sink balance record=%v %v want g1/100", sink.balanceUIDs, sink.balanceAmount)
	}
	if sink.checkinCalls != 0 {
		t.Errorf("global path must not emit a checkin ledger record: %d", sink.checkinCalls)
	}
}

// A canceled context must stop before any global balance request. Before this
// fix the global branch ran before ctx checks and still hit every global
// account after shutdown.
func TestGlobalCheckinCanceledCtxStopsBeforeBalance(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, globalResourceBody)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	s := New(Config{Pool: p, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, ChatBaseGlobal: srv.URL,
		BillingBaseCN: srv.URL, BillingBaseGlobal: srv.URL, GlobalEnabled: true,
	}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := s.checkinAll(ctx)
	if err == nil {
		t.Fatalf("canceled ctx should return error, out=%+v", out)
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err=%v want context canceled", err)
	}
	if calls != 0 {
		t.Errorf("canceled ctx reached upstream %d times", calls)
	}
}
