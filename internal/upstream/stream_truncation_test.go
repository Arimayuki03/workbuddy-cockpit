package upstream

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// Truncation must be detected even when reasoning-as-content is disabled; the
// two features are independent.
func TestStreamTruncationDetectedWithFallbackOff(t *testing.T) {
	withReasoningFallback(t, false)
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"content":"half"}}]}` + "\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw))
	if !IsTruncatedStreamError(err) {
		t.Fatalf("err=%v want truncation sentinel", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_truncated"`) {
		t.Fatalf("missing upstream_truncated error frame: %q", body)
	}
	if strings.Index(body, "upstream_truncated") > strings.Index(body, "data: [DONE]") {
		t.Errorf("truncation error must precede [DONE]: %q", body)
	}
}

// A finish_reason without [DONE] is a complete semantic turn: keep the DONE
// fallback, but do not add a false truncation error.
func TestStreamFinishWithoutDoneIsNotTruncated(t *testing.T) {
	withReasoningFallback(t, false)
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"content":"full"}}]}` + "\n\n" +
		`data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "upstream_truncated") {
		t.Errorf("complete turn marked truncated: %q", body)
	}
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("missing DONE fallback: %q", body)
	}
}

// readErrorAfter returns its data first, then a non-EOF read error. This is the
// upstream-abort shape that strings.Reader (clean EOF) cannot model.
type readErrorAfter struct {
	data string
	err  error
}

func (r *readErrorAfter) Read(p []byte) (int, error) {
	if r.data != "" {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

// A non-EOF reader error after valid frames is an upstream read interruption,
// not a clean EOF. It must surface a protocol error before [DONE] and return
// the read sentinel (handler must not record it as success).
func TestStreamUpstreamReadErrorBeforeDone(t *testing.T) {
	withReasoningFallback(t, false)
	raw := `data: {"id":"x1","choices":[{"index":0,"delta":{"content":"half"}}]}` + "\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, &readErrorAfter{data: raw, err: errors.New("connection reset by peer")})
	if !IsUpstreamReadError(err) {
		t.Fatalf("err=%v want upstream read sentinel", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_read_error"`) {
		t.Fatalf("missing upstream_read_error frame: %q", body)
	}
	if strings.Index(body, "upstream_read_error") > strings.Index(body, "data: [DONE]") {
		t.Errorf("read error must precede [DONE]: %q", body)
	}
	if strings.Contains(body, "upstream_truncated") {
		t.Errorf("read error must not be mislabeled as clean-EOF truncation: %q", body)
	}
}

var _ io.Reader = (*readErrorAfter)(nil)

// An upstream error frame is already a failure signal; do not append a second
// synthetic truncation error after it.
func TestStreamUpstreamErrorFrameNotDoublyFlagged(t *testing.T) {
	withReasoningFallback(t, false)
	raw := `data: {"error":{"message":"upstream failed","code":6004}}` + "\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "upstream_truncated") {
		t.Errorf("upstream error frame was also marked truncated: %q", body)
	}
	if !strings.Contains(body, "upstream failed") {
		t.Errorf("upstream error frame missing: %q", body)
	}
}
