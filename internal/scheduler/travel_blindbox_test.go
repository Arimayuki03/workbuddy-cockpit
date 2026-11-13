// travel_blindbox_test.go v1.25.0 G1 + §4 顺序不变量：
// 旅行状态闸门 → claim 旅行礼物 → 回读必须 idle → 开盲盒 → depart。
// 任一前置未确认（traveling / 未知状态 / claim 未确认）都不得开盒或派出。
package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// blindBoxStub 旅行 + 盲盒专用假上游。
type blindBoxStub struct {
	mu        sync.Mutex
	states    []string
	calls     []string
	claimData string

	openCount int
}

func (s *blindBoxStub) nextState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.states) == 0 {
		return "idle"
	}
	st := s.states[0]
	if len(s.states) > 1 {
		s.states = s.states[1:]
	}
	return st
}

func (s *blindBoxStub) record(call string) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
}

func (s *blindBoxStub) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *blindBoxStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			s.record("info")
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		case "/activity/growth/buddy/travel/status":
			st := s.nextState()
			s.record("status")
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"state":%q,"daily_limit_reached":false,"record_id":42}}`, st)
		case "/activity/growth/buddy/travel/claim":
			s.record("claim")
			data := s.claimData
			if data == "" {
				data = `{"reward_credit":9}`
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"data":%s}`, data)
		case "/v2/activity/growth/buddy/quota":
			s.record("quota")
			_, _ = w.Write([]byte(`{"code":0,"data":{"affordable":3,"max_open_count":2,"balance":20}}`))
		case "/v2/activity/growth/buddy/open":
			var body struct {
				Count int `json:"count"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.openCount = body.Count
			s.calls = append(s.calls, "open")
			s.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[{"instance":{"name":"喵A"}},{"instance":{"name":"喵B"}}]}}`))
		case "/activity/growth/buddy/travel/depart":
			s.record("depart")
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func wantCalls(t *testing.T, got []string, want string) {
	t.Helper()
	if strings.Join(got, ",") != want {
		t.Fatalf("calls=%v want %s", got, want)
	}
}

// TestBlindBoxIdleOrdering idle → 开盒（count=min(3,2)=2）→ depart。
func TestBlindBoxIdleOrdering(t *testing.T) {
	fastTravel(t)
	stub := &blindBoxStub{states: []string{"idle"}}
	srv := stub.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	wantCalls(t, stub.snapshot(), "info,status,quota,open,depart")
	if stub.openCount != 2 {
		t.Fatalf("open count=%d want 2", stub.openCount)
	}
}

// TestBlindBoxTravelingNoWrites traveling → 不查盲盒、不 depart（§4 闸门）。
func TestBlindBoxTravelingNoWrites(t *testing.T) {
	fastTravel(t)
	stub := &blindBoxStub{states: []string{"traveling"}}
	srv := stub.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	wantCalls(t, stub.snapshot(), "info,status")
}

// TestBlindBoxUnknownStateNoWrites 未知状态 → 封锁全部 Buddy 写。
func TestBlindBoxUnknownStateNoWrites(t *testing.T) {
	fastTravel(t)
	stub := &blindBoxStub{states: []string{"teleporting"}}
	srv := stub.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	wantCalls(t, stub.snapshot(), "info,status")
}

// TestBlindBoxArrivedClaimThenOpenThenDepart claim → 回读 idle → 开盒 → depart。
func TestBlindBoxArrivedClaimThenOpenThenDepart(t *testing.T) {
	fastTravel(t)
	stub := &blindBoxStub{states: []string{"arrived", "idle"}}
	srv := stub.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	wantCalls(t, stub.snapshot(), "info,status,claim,status,quota,open,depart")
}

// TestBlindBoxArrivedClaimUnconfirmedNoWrites claim 200 但无 reward_credit →
// 不开盒也不 depart（避免覆盖 travel record / 丢礼物）。
func TestBlindBoxArrivedClaimUnconfirmedNoWrites(t *testing.T) {
	fastTravel(t)
	stub := &blindBoxStub{states: []string{"arrived"}, claimData: `{}`}
	srv := stub.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	wantCalls(t, stub.snapshot(), "info,status,claim")
}

// TestBlindBoxArrivedStillTravelingAfterClaim claim 后状态仍非 idle → 不开盒/不派出。
func TestBlindBoxArrivedStillTravelingAfterClaim(t *testing.T) {
	fastTravel(t)
	stub := &blindBoxStub{states: []string{"arrived", "traveling"}}
	srv := stub.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	wantCalls(t, stub.snapshot(), "info,status,claim,status")
}
