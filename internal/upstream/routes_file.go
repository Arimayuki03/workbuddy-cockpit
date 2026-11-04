package upstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 独立运维工具（signin / trial / activity / credit）不经过 cmd/server 的配置
// 解析，必须自己加载 config.json 的 proxy_routes，否则账号绑定的线路会解析不到，
// 或静默从默认出口发出，造成隔离失效。
const defaultRoutesConfigPath = "/app/conf/config.json"

// ConfigPathFromEnv 返回独立二进制读取配置的路径：
// $WB2A_CONFIG → 当前目录 config.json → 容器默认 /app/conf/config.json。
func ConfigPathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_CONFIG")); v != "" {
		return v
	}
	if _, err := os.Stat("config.json"); err == nil {
		if abs, aerr := filepath.Abs("config.json"); aerr == nil {
			return abs
		}
		return "config.json"
	}
	return defaultRoutesConfigPath
}

// LoadRoutesFromFile 读取 config.json 的 proxy_routes 段并注入客户端线路表。
// 未配置该段返回空表与 nil；文件缺失/JSON 损坏返回错误，由调用方决定是否继续。
func LoadRoutesFromFile(c *Client, path string) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("upstream client is nil")
	}
	if strings.TrimSpace(path) == "" {
		path = ConfigPathFromEnv()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var doc struct {
		ProxyRoutes map[string]string `json:"proxy_routes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if err := c.SetProxyRoutes(doc.ProxyRoutes); err != nil {
		return nil, fmt.Errorf("应用配置 %s: %w", path, err)
	}
	names := make([]string, 0, len(doc.ProxyRoutes))
	for name := range doc.ProxyRoutes {
		if strings.HasPrefix(name, "_") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// LoadRoutesAndLog 是独立二进制的标准接线。三种结果都留痕：加载成功、未配置、
// 读取/解析失败；失败后工具继续运行，但绑定线路的账号会明确报错而不是静默直连。
func LoadRoutesAndLog(c *Client, logf func(string, ...any)) []string {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	names, err := LoadRoutesFromFile(c, "")
	switch {
	case err != nil:
		logf("出口线路未加载：%v（绑了线路的账号会显式报错，未绑的走默认出口）", err)
	case len(names) == 0:
		logf("出口线路：config.json 未配置 proxy_routes 段，全部账号走默认出口")
	default:
		logf("出口线路已加载：%d 条（%s）", len(names), strings.Join(names, ", "))
	}
	return names
}
