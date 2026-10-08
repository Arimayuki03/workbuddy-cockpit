// 测试辅助：JSON 序列化与临时文件读写（独立小文件，避免主测试文件工具堆积）。
package keystore

import (
	"encoding/json"
	"os"
	"testing"
)

// ptr 取值拷贝的地址（List 返回值快照转 *Key 用，模拟 Resolve 的快照形态）。
func ptr(k Key) *Key { return &k }

// mustJSON 序列化为 JSON（失败即 Fatal）。
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal err=%v", err)
	}
	return raw
}

// readTestFile 读取文件内容（失败即 Fatal）。
func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s err=%v", path, err)
	}
	return raw
}

// writeTestFile 写文件（失败即 Fatal）。
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s err=%v", path, err)
	}
}
