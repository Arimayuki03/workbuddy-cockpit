package upstream

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 小于阈值不压、超过阈值压且可解回原文（issue #297）。
func TestOutboundBodyGzipThreshold(t *testing.T) {
	c := New()
	c.GzipBody = true
	c.SyncHot()
	small := bytes.Repeat([]byte("a"), gzipBodyThreshold-1)
	if out, enc := c.outboundBody(small); enc != "" || !bytes.Equal(out, small) {
		t.Fatalf("小于阈值不应压缩（enc=%q len=%d）", enc, len(out))
	}
	big := bytes.Repeat([]byte("上下文"), 30000) // 可压缩文本，>64KB
	out, enc := c.outboundBody(big)
	if enc != "gzip" {
		t.Fatalf("超过阈值应压缩，got enc=%q", enc)
	}
	if len(out) >= len(big) {
		t.Fatalf("压缩后应更小：%d >= %d", len(out), len(big))
	}
	zr, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	back, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(back, big) {
		t.Fatal("解压后与原文不一致")
	}
}

// 关闭开关时永不压缩（排障路径）。
func TestOutboundBodyDisabled(t *testing.T) {
	c := New()
	c.GzipBody = false
	c.SyncHot()
	big := bytes.Repeat([]byte("上下文"), 30000)
	if _, enc := c.outboundBody(big); enc != "" {
		t.Fatalf("关闭后不应压缩，got %q", enc)
	}
}

// 只认 415 与「解析失败类」400。
func TestGzipUnsupported(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusUnsupportedMediaType, "", true},
		{http.StatusBadRequest, `{"msg":"Unmarshal chat params failed"}`, true},
		{http.StatusBadRequest, `{"msg":"invalid character '\x1f' looking for beginning of value"}`, true},
		{http.StatusBadRequest, `{"msg":"content-encoding not supported"}`, true},
		{http.StatusBadRequest, `{"code":11101,"msg":"bad params"}`, true},
		{http.StatusBadRequest, `{"code": 11101,"msg":"bad params"}`, true},
		{http.StatusBadRequest, `{"msg":"failed to decode request: unexpected eof"}`, true},
		{http.StatusBadRequest, `{"msg":"model is required"}`, false},
		{http.StatusTooManyRequests, `{"code":6004}`, false},
		{http.StatusInternalServerError, `oops`, false},
	}
	for _, c := range cases {
		if got := gzipUnsupported(c.status, c.body); got != c.want {
			t.Errorf("gzipUnsupported(%d, %q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// unapproved channel 只作为 gzip 探针的宽松信号，不能并入 IsChannelCheck 的强
// 判据：它与内容审核文案重叠，Classify 必须保持既有 ErrContentBlocked 语义。
func TestUnapprovedChannelIsLooseProbeOnly(t *testing.T) {
	for _, body := range []string{
		"Illegal API invocation from an unapproved channel",
		"ILLEGAL API INVOCATION FROM AN UNAPPROVED CHANNEL",
		`{"code":400,"msg":"Illegal API invocation from an unapproved channel"}`,
	} {
		if IsChannelCheck(body) {
			t.Errorf("IsChannelCheck(%q)=true; unapproved channel must stay a loose probe", body)
		}
		if !isUnapprovedChannel(body) {
			t.Errorf("isUnapprovedChannel(%q)=false want true", body)
		}
	}
	if isUnapprovedChannel(`{"code":11140,"msg":"request illegal"}`) {
		t.Error("request illegal alone must not be treated as unapproved channel")
	}
}

// 强渠道文案与 unapproved channel 同时出现时仍必须归 ErrAccountFault，走渠道
// 抢救/抓包/负缓存路径，不能被内容拦截分类抢走。
func TestClassifyStrongChannelWithUnapprovedWording(t *testing.T) {
	body := `{"code":11-128,"msg":"Illegal API invocation from an unapproved channel",` +
		`"displayTips":{"en":"This call failed channel verification."}}`
	if got := Classify(400, body); got != ErrAccountFault {
		t.Fatalf("Classify(strong channel + unapproved)=%v want ErrAccountFault", got)
	}
	bare := `{"code":11-128,"msg":"Illegal API invocation from an unapproved channel"}`
	if got := Classify(400, bare); got != ErrContentBlocked {
		t.Fatalf("Classify(bare unapproved)=%v want ErrContentBlocked", got)
	}
}

// 压缩体收到**任意 4xx**都先做一次未压缩重试（不再依赖响应文案白名单），
// 未压缩成功即返回 200；闩锁只在「未压缩成功 + 压缩版拒绝明确指向
// gzip/渠道问题」时置位（普通 400 不置，避免把真实请求错误误判成 gzip 不兼容）。
func TestGzipRetriesAny4xxThenLatchesOnlyWhenSpecific(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantLatch bool
	}{
		{"415", http.StatusUnsupportedMediaType, `{"msg":"unsupported media type"}`, true},
		{"11140 request illegal", http.StatusBadRequest, `{"code":11140,"msg":"request illegal"}`, true},
		{"11101 parse failure", http.StatusBadRequest, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, true},
		{"plain 400", http.StatusBadRequest, `{"code":400,"msg":"model is required"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var encodings []string
			c := testClient(func(r *http.Request) (*http.Response, error) {
				encodings = append(encodings, r.Header.Get("Content-Encoding"))
				if len(encodings) == 1 {
					return jsonResp(tc.status, tc.body), nil
				}
				return jsonResp(http.StatusOK, "data: [DONE]\n\n"), nil
			})
			c.GzipBody = true
			c.SyncHot()
			a := &auth.Auth{AccessToken: "at", ExpiresAt: 9999999999}
			body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
				strings.Repeat("长上下文", 20000) + `"}],"stream":true}`)

			rc, status, _, err := c.ChatStream(a, body, "", ChatMeta{})
			if err != nil {
				t.Fatalf("uncompressed retry should succeed: %v (status=%d)", err, status)
			}
			if rc != nil {
				_ = rc.Close()
			}
			if status != http.StatusOK {
				t.Fatalf("status=%d want 200", status)
			}
			if len(encodings) != 2 || encodings[0] != "gzip" || encodings[1] != "" {
				t.Fatalf("encodings=%v want [gzip \"\"] (compress then uncompressed retry)", encodings)
			}
			if got := c.gzipLatchActive(); got != tc.wantLatch {
				t.Fatalf("gzip latch=%v want %v", got, tc.wantLatch)
			}
		})
	}
}

// 压缩体 11140、未压缩仍 11140：说明是真实请求/账号问题，只重试一次且
// **不置闩锁**（否则会把真实请求错误误判成 gzip 不兼容，后续不再压缩）。
func TestGzip11140NotLatchedWhenUncompressedAlsoFails(t *testing.T) {
	var encodings []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		return jsonResp(http.StatusBadRequest, `{"code":11140,"msg":"request illegal"}`), nil
	})
	c.GzipBody = true
	c.SyncHot()
	a := &auth.Auth{AccessToken: "at", ExpiresAt: 9999999999}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("长上下文", 20000) + `"}],"stream":true}`)

	_, status, _, err := c.ChatStream(a, body, "", ChatMeta{})
	if err == nil {
		t.Fatal("both compressed and uncompressed 11140 should be returned as an error")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", status)
	}
	if len(encodings) != 2 || encodings[0] != "gzip" || encodings[1] != "" {
		t.Fatalf("encodings=%v want [gzip \"\"] (only one uncompressed retry)", encodings)
	}
	if c.gzipLatchActive() {
		t.Fatal("gzip latch must NOT be armed when the uncompressed retry also fails")
	}
}

// 压缩体命中渠道校验时先去压缩重发同一条路径（issue #303/0038），成功后不把
// 这次请求记成渠道受限。
func TestGzipChannelCheckRetriesUncompressed(t *testing.T) {
	channel := `{"code":11-128,"msg":"request illegal","displayTips":{"en":"This call failed channel verification."}}`
	var encodings []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		enc := r.Header.Get("Content-Encoding")
		encodings = append(encodings, enc)
		if enc == "gzip" {
			return jsonResp(http.StatusBadRequest, channel), nil
		}
		return jsonResp(http.StatusOK, "data: [DONE]\n\n"), nil
	})
	c.GzipBody = true
	c.SyncHot()
	a := &auth.Auth{AccessToken: "at", ExpiresAt: 9999999999}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("长上下文", 20000) + `"}],"stream":true}`)
	rc, status, _, err := c.ChatStream(a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("uncompressed retry should succeed: %v (status=%d)", err, status)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200", status)
	}
	if len(encodings) != 2 || encodings[0] != "gzip" || encodings[1] != "" {
		t.Fatalf("encodings=%v want [gzip \"\"] (compress then uncompressed retry)", encodings)
	}
}

// 上游返回 415 时去压缩重发一次；第二次仍 415 就按错误返回，不无限重试。
func TestGzipUnsupportedRetriesOnce(t *testing.T) {
	var calls int
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(http.StatusUnsupportedMediaType, `{"msg":"unsupported"}`), nil
	})
	c.GzipBody = true
	c.SyncHot()
	a := &auth.Auth{AccessToken: "at", ExpiresAt: 9999999999}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("长上下文", 20000) + `"}],"stream":true}`)
	_, status, _, err := c.ChatStream(a, body, "", ChatMeta{})
	if err == nil {
		t.Fatal("both compressed and uncompressed 415 should be returned as an error")
	}
	if status != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d want 415", status)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2 (compressed + uncompressed retry)", calls)
	}
}

// 压缩体被上游拒绝（415）后置停用闩锁：下一次调用的首次上游请求直接未压缩
// （不再每次先双发一遍压缩版）；手动让闩锁过期后恢复压缩。
func TestGzipLatchSkipsCompressionUntilExpiry(t *testing.T) {
	var encodings []string
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		enc := r.Header.Get("Content-Encoding")
		encodings = append(encodings, enc)
		if calls == 1 {
			return jsonResp(http.StatusUnsupportedMediaType, `{"msg":"unsupported"}`), nil
		}
		return jsonResp(http.StatusOK, "data: [DONE]\n\n"), nil
	})
	c.GzipBody = true
	c.SyncHot()
	a := &auth.Auth{AccessToken: "at", ExpiresAt: 9999999999}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("长上下文", 20000) + `"}],"stream":true}`)

	// 第 1 次：压缩版 415 → 同路径未压缩重试成功（并置闩锁）。
	rc, status, _, err := c.ChatStream(a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("first call: %v (status=%d)", err, status)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if len(encodings) != 2 || encodings[0] != "gzip" || encodings[1] != "" {
		t.Fatalf("first call encodings=%v want [gzip \"\"]", encodings)
	}
	if !c.gzipLatchActive() {
		t.Fatal("gzip latch must be armed after upstream rejects the compressed body")
	}

	// 第 2 次：闩锁生效，首次上游请求就是未压缩（只有一次请求，无压缩版双发）。
	encodings = nil
	rc, status, _, err = c.ChatStream(a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("latched call: %v (status=%d)", err, status)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if len(encodings) != 1 || encodings[0] != "" {
		t.Fatalf("latched call encodings=%v want [\"\"] (compression must be skipped)", encodings)
	}

	// 手动让闩锁过期：恢复压缩（首次请求重新带 Content-Encoding: gzip）。
	c.gzipMu.Lock()
	c.gzipDisabledUntil = time.Now().Add(-time.Second)
	c.gzipMu.Unlock()
	encodings = nil
	rc, status, _, err = c.ChatStream(a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("post-expiry call: %v (status=%d)", err, status)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if len(encodings) != 1 || encodings[0] != "gzip" {
		t.Fatalf("post-expiry encodings=%v want [gzip] (compression must resume)", encodings)
	}
}

// 压缩体命中渠道校验（11-128）同样置闩锁：下一次调用的首次上游请求直接未压缩。
func TestGzipLatchAfterChannelCheck(t *testing.T) {
	channel := `{"code":11-128,"msg":"request illegal","displayTips":{"en":"This call failed channel verification."}}`
	var encodings []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		enc := r.Header.Get("Content-Encoding")
		encodings = append(encodings, enc)
		if enc == "gzip" {
			return jsonResp(http.StatusBadRequest, channel), nil
		}
		return jsonResp(http.StatusOK, "data: [DONE]\n\n"), nil
	})
	c.GzipBody = true
	c.SyncHot()
	a := &auth.Auth{AccessToken: "at", ExpiresAt: 9999999999}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("长上下文", 20000) + `"}],"stream":true}`)

	rc, status, _, err := c.ChatStream(a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("first call: %v (status=%d)", err, status)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if len(encodings) != 2 || encodings[0] != "gzip" || encodings[1] != "" {
		t.Fatalf("first call encodings=%v want [gzip \"\"]", encodings)
	}
	if !c.gzipLatchActive() {
		t.Fatal("gzip latch must be armed after the compressed body hits channel check")
	}

	encodings = nil
	rc, status, _, err = c.ChatStream(a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("latched call: %v (status=%d)", err, status)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if len(encodings) != 1 || encodings[0] != "" {
		t.Fatalf("latched call encodings=%v want [\"\"] (compression must be skipped)", encodings)
	}
}
