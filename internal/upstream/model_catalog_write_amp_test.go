package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// write-amp 回归（lookup 命中点写放大修复）：
// MaxOutputTokensListingV4 对「models.dev 命中但 output=0」的模型，每次 /v1/models
// 都会走到第 4 级 lookup 命中（第 3 级只认 MaxOutputTokens>0 的缓存条目，恒 miss）。
// 旧实现无条件 modelCatalogPut → 整文件 MarshalIndent+写盘，写放大随请求无界。
// 修复后走 modelCatalogPutIfEnrich：首次写入后同值跳过，落盘恰好一次。
//
// 判定用 saveLocked 计数钩子（modelCatalogSaveHook）：Windows mtime 粒度粗，
// 快速连写会同刻，mtime 判定不可靠；计数在持锁的 saveLocked 内递增，精确无歧义。
func TestMaxOutputTokensListingV4NoWriteAmplification(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	dir := t.TempDir()
	loadModelCatalogAt(filepath.Join(dir, "model.json"))

	var saves int64
	modelCatalogSaveHook = func() { saves++ }
	t.Cleanup(func() { modelCatalogSaveHook = nil })

	// 场景一：models.dev 命中但 output=0（写放大主形态：output 链每请求都进第 4 级）。
	modelsDev.mu.Lock()
	modelsDev.doc = map[string]modelsDevEntry{
		"zero-output-model": {Context: 100000, Output: 0},
	}
	modelsDev.fetched = true
	modelsDev.lastFetch = time.Now()
	modelsDev.mu.Unlock()

	const rounds = 50
	for i := 0; i < rounds; i++ {
		if got := ContextWindowListingV4("zero-output-model", 0, nil); got != 100000 {
			t.Fatalf("round %d context: %d want 100000", i, got)
		}
		if got, ok := MaxOutputTokensListingV4("zero-output-model", 0, nil); ok || got != 0 {
			t.Fatalf("round %d output: %d,%v want 0,false", i, got, ok)
		}
	}
	if saves != 1 {
		t.Errorf("output=0 模型 %d 轮 ListingV4 落盘 %d 次 want 1（首次写入后同值跳过，写放大回归）", rounds, saves)
	}
	// 落盘文件恰好含该条目且值正确（写盘语义未被破坏，只是不再重复）。
	raw, err := os.ReadFile(filepath.Join(dir, "model.json"))
	if err != nil {
		t.Fatalf("model.json not persisted: %v", err)
	}
	var file map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("model.json corrupted: %v", err)
	}
	if e := file["zero-output-model"]; e.ContextLength != 100000 || e.MaxOutputTokens != 0 || e.Source != "modelsdev" {
		t.Errorf("persisted entry: %+v want 100000/0/modelsdev", e)
	}

	// 场景二：缓存条目 MaxOutput=0 而 models.dev 有正值 → 补值恰好一次落盘，随后稳定。
	modelsDev.mu.Lock()
	modelsDev.doc["enrich-model"] = modelsDevEntry{Context: 200000, Output: 32768}
	modelsDev.mu.Unlock()
	modelCatalogPut("enrich-model", 200000, 0) // 预置「输出上限未知」缓存条目（含落盘）
	saves = 0
	for i := 0; i < rounds; i++ {
		if got := ContextWindowListingV4("enrich-model", 0, nil); got != 200000 {
			t.Fatalf("enrich round %d context: %d want 200000", i, got)
		}
		got, ok := MaxOutputTokensListingV4("enrich-model", 0, nil)
		if !ok || got != 32768 {
			t.Fatalf("enrich round %d output: %d,%v want 32768,true", i, got, ok)
		}
	}
	if saves != 1 {
		t.Errorf("补值形态 %d 轮 ListingV4 落盘 %d 次 want 1（0→正值补一次，此后第 3 级命中）", rounds, saves)
	}
}
