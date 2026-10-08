package server

import (
	"math"
	"testing"
	"time"
)

// tokensPerSecForRecord 单请求聚合后的 tok/s：走 recordChatMetric 全路径
// （生成分母在聚合层计算，出口 TokensPerSec = compTok/genSecSum），比单测内部
// 表达式更能钉住口径——分母算法改动（如漏扣/错扣 TTFB）会直接红在这里。
func tokensPerSecForRecord(t *testing.T, toks int, total, ttfb time.Duration) float64 {
	t.Helper()
	resetMetricsForTest(t)
	recordChatMetric(&chatStat{
		model: "m", mode: "stream", status: 200,
		ttfb: ttfb, toks: toks, hasUsage: true,
	}, total)
	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 {
		t.Fatalf("models=%d want 1", len(snap.Models))
	}
	return snap.Models[0].TokensPerSec
}

// TestMetricsTokensPerSecond tok/s 的分母应是「生成耗时」= 端到端 - TTFB。
//
// 这条测试的价值在于把"没有观测就不扣"与"扣出来的窗口可信才用"两条都钉住：
//   - 非流式回复没有首个 data 帧（ttfb=0），若实现改成"没测到就按某个默认 TTFB
//     扣"，速率会虚高，而单看代码很难发现；
//   - 上游攒批下发（假流式）时首帧与末帧几乎同时到，total−ttfb 只剩毫秒级，
//     拿它当分母会把几百 token 除成上万 tok/s 的幻数（面板 issues #127）。
func TestMetricsTokensPerSecond(t *testing.T) {
	cases := []struct {
		name      string
		tokens    int
		total     time.Duration
		ttfb      time.Duration
		want      float64
		tolerance float64
	}{
		{"无 TTFB 观测（非流式）按端到端算", 100, 2 * time.Second, 0, 50, 0.01},
		{"扣掉 TTFB：1200ms 里等了 200ms", 100, 1200 * time.Millisecond, 200 * time.Millisecond, 100, 0.01},
		{"TTFB 占总耗时一半", 50, time.Second, 500 * time.Millisecond, 100, 0.01},
		{"TTFB 超过总耗时 → 退回端到端，不得负/零分母", 100, 100 * time.Millisecond, 500 * time.Millisecond, 1000, 0.01},
		{"TTFB 恰好等于总耗时 → 退回端到端", 100, 100 * time.Millisecond, 100 * time.Millisecond, 1000, 0.01},
		// 攒批下发/假流式护栏：扣除后剩余窗口不足 200ms 即视为生成时长不可测，
		// 退回端到端耗时，不得除出上万 tok/s 的幻数。
		{"扣除后只剩 50ms（攒批下发）→ 退回端到端", 500, 5 * time.Second, 4950 * time.Millisecond, 100, 0.01},
		{"扣除后不足 200ms 下限 → 退回端到端", 100, time.Second, 950 * time.Millisecond, 100, 0.01},
		{"恰好达到 200ms 下限 → 照常扣除", 100, time.Second, 800 * time.Millisecond, 500, 0.01},
		{"零耗时（无分母）", 10, 0, 0, 0, 0.01},
		{"零 token 但有效耗时", 0, time.Second, 0, 0, 0.01},
	}
	for _, c := range cases {
		got := tokensPerSecForRecord(t, c.tokens, c.total, c.ttfb)
		if math.Abs(got-c.want) > c.tolerance {
			t.Errorf("%s: got %.3f want %.3f", c.name, got, c.want)
		}
	}
}

// TestMetricsTokensPerSecondTTFBMonotonic 语义回归：TTFB 越大，扣减越多、速率越高。
// 若实现退化为端到端口径（漏扣 TTFB），三者会相等——这条就会红。
func TestMetricsTokensPerSecondTTFBMonotonic(t *testing.T) {
	const tok = 200
	total := 2 * time.Second
	without := tokensPerSecForRecord(t, tok, total, 0)
	with200 := tokensPerSecForRecord(t, tok, total, 200*time.Millisecond)
	with800 := tokensPerSecForRecord(t, tok, total, 800*time.Millisecond)

	if !(without < with200 && with200 < with800) {
		t.Fatalf("速率应随 TTFB 增大而上升：ttfb=0 -> %.2f, 200ms -> %.2f, 800ms -> %.2f",
			without, with200, with800)
	}
	if math.Abs(without-100) > 0.01 {
		t.Errorf("无 TTFB 时应等于端到端口径 100，得到 %.2f", without)
	}
	if math.Abs(with200-111.11) > 0.05 {
		t.Errorf("扣 200ms 后应约 111.11，得到 %.2f", with200)
	}
	if math.Abs(with800-166.67) > 0.05 {
		t.Errorf("扣 800ms 后应约 166.67，得到 %.2f", with800)
	}
}

// TestMetricsGenWindowGuardrail 在聚合层直接钉 200ms 护栏边界：
// 剩余窗口 199ms 退回端到端、201ms 照常扣除（毫秒粒度下的边界两侧）。
func TestMetricsGenWindowGuardrail(t *testing.T) {
	// total=1s, ttfb=801ms → 剩余 199ms < 200ms → 退回端到端 1s。
	if got := tokensPerSecForRecord(t, 100, time.Second, 801*time.Millisecond); math.Abs(got-100) > 0.01 {
		t.Errorf("剩余 199ms 应退回端到端（tok/s=100），得到 %.3f", got)
	}
	// total=1s, ttfb=799ms → 剩余 201ms >= 200ms → 照常扣除（100/0.201≈497.5）。
	if got := tokensPerSecForRecord(t, 100, time.Second, 799*time.Millisecond); !(got > 400 && got < 600) {
		t.Errorf("剩余 201ms 应照常扣除（tok/s≈497.5），得到 %.3f", got)
	}
}
