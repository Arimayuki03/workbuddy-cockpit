package server

import (
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestApplyErrorPolicyUpstreamRestrictionReason(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})
	body := `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "glm-5.2", nil)
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("风控封禁应禁用: %+v", st)
	}
	if !strings.Contains(st.DisabledReason, "appeal required") ||
		strings.Contains(st.DisabledReason, "re-login required") {
		t.Fatalf("disabled_reason=%q", st.DisabledReason)
	}
}

func TestApplyErrorPolicyPlain11140StillRelogin(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})
	body := `{"code":11140,"msg":"request illegal"}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "glm-5.2", nil)
	st, _ := p.Status("u1")
	if !st.Disabled || !strings.Contains(st.DisabledReason, "re-login required") {
		t.Fatalf("普通 11140 应提示重登: %+v", st)
	}
}
