package upstream

import "sync/atomic"

// reasoningAsContent is the process-wide switch for the reasoning-only
// compatibility fallback. It is deliberately off by default: clients that
// render both reasoning_content and content would otherwise display the same
// text twice. Deployments that use a content-only client can opt in via
// reasoning_as_content=true.
var reasoningAsContent atomic.Bool

// ConfigureReasoningFallback sets the fallback switch. Called once at startup;
// tests may toggle it as long as they restore the previous value.
func ConfigureReasoningFallback(on bool) {
	reasoningAsContent.Store(on)
}

// ReasoningFallbackEnabled reports whether the fallback is enabled.
func ReasoningFallbackEnabled() bool {
	return reasoningAsContent.Load()
}
