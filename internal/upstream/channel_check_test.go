package upstream

import (
	"net/http"
	"strings"
	"testing"
)

// The upstream channel check has two observed code spellings. Keep both as
// concatenations so a typo cannot silently weaken the predicate.
var (
	channelCheckCodeDashed = "1" + "1" + "-" + "1" + "2" + "8"
	channelCheckCodePlain  = "1" + "1" + "1" + "2" + "8"
)

func channelCheckTestBody(code string) string {
	return `{"code":` + code + `,"msg":"request illegal","requestId":"x",` +
		`"displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。",` +
		`"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`
}

func TestIsChannelCheckMatches(t *testing.T) {
	if !IsChannelCheck(channelCheckTestBody(channelCheckCodeDashed)) {
		t.Fatal("dashed channel-check code was not recognized")
	}
	for _, code := range []string{
		channelCheckCodeDashed,
		channelCheckCodePlain,
		`"` + channelCheckCodeDashed + `"`,
	} {
		body := `{"code":` + code + `,"displayTips":{"en":"This call failed channel verification"}}`
		if !IsChannelCheck(body) {
			t.Errorf("channel-check code form %s was not recognized", code)
		}
	}
	if !IsChannelCheck(`{"code":` + channelCheckCodePlain + `,"displayTips":{"en":"possibly because it was not sent from an official client"}}`) {
		t.Error("English official-client wording was not recognized")
	}
	// Strong channel wording without a code still must be recognized, otherwise
	// a `request illegal` envelope could disable a healthy account.
	if !IsChannelCheck(`{"msg":"request illegal","displayTips":{"zh":"这次调用没通过渠道校验"}}`) {
		t.Error("channel wording without a business code was not recognized")
	}
	if !IsChannelCheck(`{"msg":"request illegal","displayTips":{"en":"This call failed channel verification"}}`) {
		t.Error("English channel wording without a business code was not recognized")
	}
}

// A channel check may carry the historical `request illegal` message, but a
// plain 11140 account ban without channel-check wording must remain an account
// fault. The regex must also not swallow content-policy 11-128 responses.
func TestIsChannelCheckIsConservative(t *testing.T) {
	if IsChannelCheck(`{"msg":"request illegal"}`) {
		t.Error("request illegal alone must not be classified as a channel check")
	}
	content := `{"code":` + channelCheckCodeDashed + `,"msg":"blocked by security policy"}`
	if IsChannelCheck(content) {
		t.Fatal("content moderation must not be classified as a channel check")
	}
	if got := Classify(http.StatusBadRequest, content); got != ErrContentBlocked {
		t.Errorf("content moderation classification changed: got %v want ErrContentBlocked", got)
	}
}

// Handler's ErrAccountFault branch handles channel checks first; keeping the
// classification account-level is what routes it there.
func TestClassifyChannelCheckIsAccountFault(t *testing.T) {
	body := channelCheckTestBody(channelCheckCodePlain)
	if got := Classify(http.StatusBadRequest, body); got != ErrAccountFault {
		t.Errorf("channel check Classify=%v want ErrAccountFault", got)
	}
	if hint := GatewayHint(ErrAccountFault, body, HintContext{}); !strings.Contains(hint, "channel verification") || strings.Contains(hint, "disable this account") {
		t.Errorf("channel-check hint should describe soft handling, got %q", hint)
	}
}

// 弱 11-128 封套（只有业务码 + request illegal，没有强渠道文案）不能落到
// accountFaultRule 被硬禁用；归请求级 ErrContentBlocked，走内容拦截/降级。
func TestClassifyWeakChannelCodeIsRequestLevel(t *testing.T) {
	for _, code := range []string{
		channelCheckCodeDashed,
		channelCheckCodePlain,
		`"` + channelCheckCodeDashed + `"`,
	} {
		body := `{"code":` + code + `,"msg":"request illegal","requestId":"r1"}`
		if got := Classify(http.StatusBadRequest, body); got != ErrContentBlocked {
			t.Errorf("Classify(weak 11-128 code=%s)=%v want ErrContentBlocked", code, got)
		}
	}
}

func TestClassifyEmptyMessage(t *testing.T) {
	bodies := []string{
		`{"code":11151,"msg":"a message has empty content, please check the conversation history and retry","requestId":"r1"}`,
		`{"code":"11151","msg":"empty"}`,
		`{"code": 11151,"msg":"x"}`,
		`{"msg":"a message has empty content"}`,
		`{"code":0,"msg":"存在空白消息，请补全后重试"}`,
	}
	for _, body := range bodies {
		if got := Classify(http.StatusBadRequest, body); got != ErrEmptyMessage {
			t.Errorf("Classify(%q)=%v want ErrEmptyMessage", body, got)
		}
	}
	if got := Classify(http.StatusBadRequest, `{"code":`+channelCheckCodeDashed+`,"msg":"blocked by security policy"}`); got == ErrEmptyMessage {
		t.Error("content moderation was misclassified as an empty message")
	}
}
