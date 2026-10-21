package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// serverReadErrorReader returns its data first, then a non-EOF read error.
// This models an upstream/proxy connection reset after partial SSE output —
// the shape strings.Reader (clean EOF) cannot exercise.
type serverReadErrorReader struct {
	data string
	err  error
}

func (r *serverReadErrorReader) Read(p []byte) (int, error) {
	if r.data != "" {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *serverReadErrorReader) Close() error { return nil }

// An upstream non-EOF read error must be treated as upstream failure (502 in
// logs/metrics, no NoteSuccess/stickiness), not as a client disconnect. The
// client still receives a protocol error before [DONE].
func TestChatStreamUpstreamReadErrorLogged502(t *testing.T) {
	withChatLog(t)
	const partial = "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n"
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &serverReadErrorReader{data: partial, err: errors.New("connection reset by peer")},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("wire code=%d want 200 (headers already committed)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_read_error"`) {
		t.Fatalf("client must receive read-error event: %s", body)
	}
	if strings.Index(body, "upstream_read_error") > strings.Index(body, "data: [DONE]") {
		t.Errorf("read error must precede [DONE]: %s", body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("upstream read error must be logged as 502, got:\n%s", out)
	}
	st, _ := p.Status("u1")
	if st.SuccessCount != 0 {
		t.Errorf("upstream read error must not be recorded as success: %+v", st)
	}
	if st.Cooling || st.Disabled {
		t.Errorf("transport read error must not penalize the account: %+v", st)
	}
}
