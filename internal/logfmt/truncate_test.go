package logfmt

import (
	"strings"
	"testing"
)

// TestTruncateASCII ASCII 截断：按字节上限正常切。
func TestTruncateASCII(t *testing.T) {
	// Arrange
	s := "abcdefghijklmnopqrstuvwxyz"

	// Act
	got := Truncate(s, 10)

	// Assert
	if got != "abcdefghij" {
		t.Errorf("Truncate(ascii, 10)=%q want %q", got, "abcdefghij")
	}
}

// TestTruncateCJKBoundary 中文截断：字节切点落在多字节字符中间时回退到 rune
// 边界，不出乱码（半截 UTF-8 序列）。
func TestTruncateCJKBoundary(t *testing.T) {
	// Arrange
	s := "将在 24 小时后重置限额" // 每个汉字 3 字节
	// Act
	got := Truncate(s, 4) // 切点落在第 2 个汉字（字节 3..6）中间 → 应回退到 3 字节边界

	// Assert
	if got != "将" {
		t.Errorf("Truncate(cjk, 4)=%q want %q（回退到 rune 边界）", got, "将")
	}
	for _, r := range got {
		if r == 0xFFFD { // utf8.RuneError：半截序列解码失败的替换字符
			t.Fatalf("Truncate 输出含乱码替换字符: %q", got)
		}
	}
}

// TestTruncateShorterThanN 短于 n 的输入原样返回（不补不截）。
func TestTruncateShorterThanN(t *testing.T) {
	// Arrange
	s := "短"

	// Act + Assert
	if got := Truncate(s, 100); got != s {
		t.Errorf("Truncate(短, 100)=%q want 原样 %q", got, s)
	}
	if got := Truncate("", 10); got != "" {
		t.Errorf("Truncate(\"\", 10)=%q want 空串", got)
	}
}

// TestTruncateTrimSpace 与旧 client.go 实现口径对齐：先 TrimSpace 再截断。
func TestTruncateTrimSpace(t *testing.T) {
	// Arrange
	s := "  error body  "

	// Act + Assert
	if got := Truncate(s, 20); got != "error body" {
		t.Errorf("Truncate(%q, 20)=%q want %q（TrimSpace 后原样）", s, got, "error body")
	}
}

// TestTruncateASCIIStableForExactByte 多字节串在 rune 边界内整段保留：
// 切点恰好落在 rune 边界时按上限整切。
func TestTruncateASCIIStableForExactByte(t *testing.T) {
	// Arrange
	s := strings.Repeat("中", 5) // 15 字节

	// Act + Assert
	if got := Truncate(s, 9); got != strings.Repeat("中", 3) {
		t.Errorf("Truncate(中×5, 9)=%q want 中×3（9=3 个 rune 的字节边界）", got)
	}
}

// TestTruncateWidth 按**显示列宽**截断：CJK 一个 rune 占 2 列，字节口径做不到
// 「截完必 ≤ width」——这是定宽表格列（chat 流水行账号列）的对齐前提。
func TestTruncateWidth(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"ascii 直切", "173****6289", 8, "173****6"},
		{"cjk 按列宽切", "蝶舞飞扬昵称", 8, "蝶舞飞扬"},
		{"cjk 落单列不保留", "蝶舞", 3, "蝶"}, // 第 2 个汉字需 2 列，只剩 1 列 → 让出
		{"不超宽原样", "多喝水", 8, "多喝水"},
		{"width 0 空串", "abc", 0, ""},
		{"width 负数空串", "abc", -1, ""},
		{"恰好等宽", "多喝水", 6, "多喝水"},
	}
	for _, c := range cases {
		got := TruncateWidth(c.in, c.width)
		if got != c.want {
			t.Errorf("%s: TruncateWidth(%q,%d)=%q want %q", c.name, c.in, c.width, got, c.want)
		}
		// 不变量（仅正宽度）：截完必 ≤ width（对齐承诺），且无 rune 切断（无替换字符）。
		if c.width > 0 {
			if DisplayWidth(got) > c.width {
				t.Errorf("%s: 结果显示宽 %d 超 width %d", c.name, DisplayWidth(got), c.width)
			}
			for _, r := range got {
				if r == 0xFFFD {
					t.Fatalf("%s: 输出含乱码替换字符 %q", c.name, got)
				}
			}
		}
	}
}
