package upstream

import (
	"strings"
	"testing"
)

// The upstream channel treats the DSH self-introduction sentence as a
// fingerprint regardless of the brand name substituted into it. Rewriting the
// whole sentence (not just a brand token) must leave no fragment behind.
func TestDshSystemPromptSentenceRewritten(t *testing.T) {
	cases := []string{
		"You are an AI agent powered by DeepSeek Harness.",
		"You are a coding agent powered by the deepseek-v4.1-flash model.",
		"You are an AI assistant built on DeepSeek Harness.",
		"You are an assistant built on the deepseek-v4.1-flash model.",
	}
	for _, in := range cases {
		if out := sanitizeText(in); out != "You are a helpful AI assistant." {
			t.Errorf("DSH sentence not replaced: %q -> %q", in, out)
		}
	}
}

// hasFingerprint is case-sensitive for the legacy feature list, while DSH
// requests often spell "AI" in upper case. The fast precheck must still route
// these through the rewrite layer.
func TestDshFingerprintPrecheckCaseInsensitive(t *testing.T) {
	in := "YOU ARE AN AI AGENT POWERED BY DEEPSEEK HARNESS."
	if !hasFingerprint(in) {
		t.Fatal("fast precheck missed upper-case DSH sentence")
	}
	if out := sanitizeText(in); out != "You are a helpful AI assistant." {
		t.Errorf("upper-case DSH sentence not rewritten: %q", out)
	}
	if !hasFingerprint("you are an ai agent powered by x.") {
		t.Error("fast precheck missed lower-case DSH sentence")
	}
}

// A dot inside the model name (deepseek-v4.1-flash) must not become the end of
// the match. The replacement stops at the sentence-ending dot and keeps all
// following instructions.
func TestDshModelNameWithDotsAndFollowingText(t *testing.T) {
	in := "You are a coding agent powered by the deepseek-v4.1-flash model.\nUse the read tool to inspect files.\nDo not guess."
	out := sanitizeText(in)
	if strings.Contains(out, "1-flash") || strings.Contains(out, "model.") {
		t.Errorf("sentence only partially replaced: %q -> %q", in, out)
	}
	if !strings.Contains(out, "Use the read tool to inspect files.\nDo not guess.") {
		t.Errorf("following instructions were changed: %q", out)
	}
	if got := sanitizeText("You are an AI agent powered by DeepSeek Harness. Then answer briefly."); !strings.Contains(got, "assistant. Then answer briefly.") {
		t.Errorf("sentence whitespace was swallowed: %q", got)
	}
}

// PrepareBodyOpt is the outbound boundary; no DSH fingerprint may survive it.
func TestPrepareBodySanitizesDshPrompt(t *testing.T) {
	in := "You are an AI agent powered by DeepSeek Harness."
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"` + in + `"}]}`)
	out := PrepareBodyOpt(body, true)
	if strings.Contains(string(out), "powered by DeepSeek Harness") {
		t.Errorf("outbound body still contains DSH fingerprint: %s", out)
	}
	if !strings.Contains(string(out), "helpful AI assistant") {
		t.Errorf("replacement text missing from outbound body: %s", out)
	}
}

// Idempotence matters because the gateway preprocesses the same body more than
// once (PrepareBody and ChatStream).
func TestDshRewriteIdempotent(t *testing.T) {
	once := sanitizeText("You are an AI agent powered by DeepSeek Harness.")
	if twice := sanitizeText(once); twice != once {
		t.Errorf("rewrite is not idempotent: %q -> %q", once, twice)
	}
}

// Do not broaden the regex into ordinary prose.
func TestDshPlainTextNotTouched(t *testing.T) {
	for _, in := range []string{
		"You are a helpful assistant.",
		"You are a helpful AI assistant built for coding tasks",
		"The agent powered by the harness failed.",
		"please use main branch for this repo",
	} {
		if out := sanitizeText(in); out != in {
			t.Errorf("unexpected rewrite: %q -> %q", in, out)
		}
	}
}
