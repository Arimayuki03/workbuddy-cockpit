package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestInFlightByModelObserveAndClear 每模型在途台账（观测）：AcquireModel 计入、
// ReleaseModel 归账并删行、全释放台账消失（nil）、空 model 退化 Acquire 不进台账。
func TestInFlightByModelObserveAndClear(t *testing.T) {
	p := New("")
	uid := "u-ifm"
	p.Add(&auth.Auth{UID: uid})
	if !p.AcquireModel(uid, "glm-4.6") || !p.AcquireModel(uid, "glm-4.6") || !p.AcquireModel(uid, "glm-4.5") {
		t.Fatal("acquire failed")
	}
	st, ok := p.Status(uid)
	if !ok || st.InFlight != 3 {
		t.Fatalf("in_flight=%d ok=%v want 3", st.InFlight, ok)
	}
	if st.InFlightByModel["glm-4.6"] != 2 || st.InFlightByModel["glm-4.5"] != 1 {
		t.Fatalf("ifm=%v", st.InFlightByModel)
	}
	// glm-4.5 全释放 → 删行；glm-4.6 释放一个 → 余 1
	p.ReleaseModel(uid, "glm-4.5")
	p.ReleaseModel(uid, "glm-4.6")
	time.Sleep(20 * time.Millisecond)
	st, _ = p.Status(uid)
	if _, gone := st.InFlightByModel["glm-4.5"]; gone {
		t.Fatalf("glm-4.5 should be deleted, got %v", st.InFlightByModel)
	}
	if st.InFlightByModel["glm-4.6"] != 1 {
		t.Fatalf("glm-4.6 want 1, got %v", st.InFlightByModel)
	}
	// 全释放 → 台账整体消失
	p.ReleaseModel(uid, "glm-4.6")
	st, _ = p.Status(uid)
	if st.InFlight != 0 || st.InFlightByModel != nil {
		t.Fatalf("final: in_flight=%d ifm=%v", st.InFlight, st.InFlightByModel)
	}
	// 空 model 退化 Acquire/Release：计数走通、不进台账
	if !p.AcquireModel(uid, "") {
		t.Fatal("acquire empty model")
	}
	st, _ = p.Status(uid)
	if st.InFlight != 1 || st.InFlightByModel != nil {
		t.Fatalf("empty model: in_flight=%d ifm=%v", st.InFlight, st.InFlightByModel)
	}
	p.ReleaseModel(uid, "")
	if st, _ := p.Status(uid); st.InFlight != 0 {
		t.Fatalf("release empty: in_flight=%d", st.InFlight)
	}
}
