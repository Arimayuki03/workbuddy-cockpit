package upstream

import (
	"strings"
	"testing"
)

// Error.Msg 只保留前 200 字符，渠道校验文案可能在截断点之后。HintContext.ChannelCheck
// 是分类时基于完整 body 算出的权威标记，必须优先于重新解析截断后的 Msg。
func TestGatewayHintChannelCheckFlagWithTruncatedMsg(t *testing.T) {
	full := `{"code":11-128,"msg":"request illegal","requestId":"x","displayTips":{"en":"` +
		strings.Repeat("x", 300) + ` This call failed channel verification."}}`
	truncated := full[:200]
	if IsChannelCheck(truncated) {
		t.Fatal("fixture must keep the channel marker beyond the truncation point")
	}
	got := GatewayHint(ErrAccountFault, truncated, HintContext{ChannelCheck: true})
	if !strings.Contains(got, "channel verification") || !strings.Contains(got, "account availability is unchanged") {
		t.Fatalf("must use the channel-check hint: %q", got)
	}
	if strings.Contains(got, "will rotate or disable") {
		t.Fatalf("must not fall back to the generic account-fault hint: %q", got)
	}
	// 没有标记时保持旧行为（截断后无法识别 → 通用账号故障文案）。
	if got := GatewayHint(ErrAccountFault, truncated, HintContext{}); !strings.Contains(got, "account-level fault") {
		t.Fatalf("without the flag the generic hint is expected: %q", got)
	}
}
