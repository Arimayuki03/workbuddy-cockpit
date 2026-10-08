// config_security_logging_test.go security 段 / pool.credit_floor / logging 段的
// 配置加载与归一化测试（统一接线批次）：默认值零回归、env 覆盖、trusted CIDR 非法
// fail-fast。段内各键的运行期行为由 internal/server（iprules/reqlog）与 internal/pool
// 各自的测试覆盖，此处只验证配置链路。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecurityLoggingDefaults 零配置默认值：无规则（空表 + 模式关）、hops=1、
// 保底关闭、归档关闭、来源采集关闭——与 v1.15.1 行为逐位一致。
func TestSecurityLoggingDefaults(t *testing.T) {
	c := Default()
	c.APIKey = "k"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(c.Security.TrustedProxyCIDRs) != 0 {
		t.Errorf("trusted_proxy_cidrs default must be empty (no trust), got %v", c.Security.TrustedProxyCIDRs)
	}
	if c.Security.TrustedProxyHops != 1 {
		t.Errorf("trusted_proxy_hops default = %d want 1", c.Security.TrustedProxyHops)
	}
	if len(c.Security.IPBlacklist) != 0 || len(c.Security.IPWhitelist) != 0 || c.Security.IPWhitelistMode {
		t.Errorf("ip rules default must be empty/disabled: %+v", c.Security)
	}
	if c.Pool.CreditFloor != 0 {
		t.Errorf("credit_floor default = %d want 0 (off)", c.Pool.CreditFloor)
	}
	if c.Logging.RequestArchive || c.Logging.RequestClientInfo {
		t.Errorf("logging defaults must be off: %+v", c.Logging)
	}
	if c.Logging.RequestRetentionDays != 0 || c.Logging.RequestArchiveMaxMB != 0 {
		// 缺省 0 由 reqlog 侧回落默认（7 天 / 200MB），config 层不硬置。
		t.Errorf("retention/max_mb default = %d/%d want 0 (reqlog-side fallback)", c.Logging.RequestRetentionDays, c.Logging.RequestArchiveMaxMB)
	}
}

// TestSecurityLoadAndEnv security 段文件加载 + WB2A_SECURITY_* env 覆盖（逗号分隔
// 数组）；credit_floor 与 logging 段 env 覆盖一并验证。
func TestSecurityLoadAndEnv(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","security":{"trusted_proxy_cidrs":["127.0.0.1","10.0.0.0/8"],"trusted_proxy_hops":2,"ip_blacklist":["203.0.113.9"],"ip_whitelist_mode":true},"pool":{"credit_floor":300},"logging":{"request_archive":true,"request_retention_days":14,"request_archive_max_mb":500,"request_client_info":true}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Security.TrustedProxyCIDRs) != 2 {
		t.Fatalf("trusted cidrs = %v want 2 entries", c.Security.TrustedProxyCIDRs)
	}
	if c.Security.TrustedProxyHops != 2 || !c.Security.IPWhitelistMode {
		t.Errorf("security=%+v", c.Security)
	}
	if c.Pool.CreditFloor != 300 || !c.Logging.RequestArchive || c.Logging.RequestRetentionDays != 14 || c.Logging.RequestArchiveMaxMB != 500 || !c.Logging.RequestClientInfo {
		t.Errorf("pool/logging=%+v / %+v", c.Pool, c.Logging)
	}

	t.Setenv("WB2A_API_KEY", "envkey") // normalize：api_key 无条件必填（无文件路径场景）
	t.Setenv("WB2A_SECURITY_IP_BLACKLIST", "198.51.100.1, 198.51.100.0/24")
	t.Setenv("WB2A_SECURITY_TRUSTED_PROXY_HOPS", "3")
	t.Setenv("WB2A_SECURITY_TRUSTED_PROXY_CIDRS", "192.168.1.1,192.168.0.0/16")
	t.Setenv("WB2A_SECURITY_IP_WHITELIST", "10.0.0.1")
	t.Setenv("WB2A_SECURITY_IP_WHITELIST_MODE", "true")
	t.Setenv("WB2A_POOL_CREDIT_FLOOR", "250")
	t.Setenv("WB2A_LOGGING_REQUEST_CLIENT_INFO", "true")
	cEnv, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cEnv.Security.IPBlacklist; len(got) != 2 || got[0] != "198.51.100.1" {
		t.Errorf("env blacklist = %v want 2 trimmed entries", got)
	}
	if cEnv.Security.TrustedProxyHops != 3 || len(cEnv.Security.TrustedProxyCIDRs) != 2 {
		t.Errorf("env hops/cidrs = %d / %v", cEnv.Security.TrustedProxyHops, cEnv.Security.TrustedProxyCIDRs)
	}
	if len(cEnv.Security.IPWhitelist) != 1 || !cEnv.Security.IPWhitelistMode {
		t.Errorf("env whitelist = %v mode=%v", cEnv.Security.IPWhitelist, cEnv.Security.IPWhitelistMode)
	}
	if cEnv.Pool.CreditFloor != 250 {
		t.Errorf("env credit_floor = %d want 250", cEnv.Pool.CreditFloor)
	}
	if !cEnv.Logging.RequestClientInfo {
		t.Errorf("env request_client_info = false want true")
	}
}

// TestSecurityBadTrustedCIDRFailFast 可信代理网段非法 → 启动 fail-fast（信任模型
// 配错静默降级不可接受）。
func TestSecurityBadTrustedCIDRFailFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","security":{"trusted_proxy_cidrs":["not-an-ip"]}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("invalid trusted_proxy_cidrs must fail config load")
	}
}

// TestCreditFloorNegativeClamped credit_floor 负值非法，归一化钳 0（不给"配错=全池
// 硬 503"留口子）。
func TestCreditFloorNegativeClamped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","pool":{"credit_floor":-5}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.CreditFloor != 0 {
		t.Errorf("negative credit_floor must clamp to 0, got %d", c.Pool.CreditFloor)
	}
}
