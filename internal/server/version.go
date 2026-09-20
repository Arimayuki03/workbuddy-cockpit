package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// appVersion 当前程序版本。由构建注入（-ldflags "-X ...version.appVersion=v1.2.0"）；
// 未注入时回落 dev（面板显示「开发版」，版本检查仍可用 latest 判断）。
var appVersion = "dev"

// AppVersion 返回当前版本号（panel 面板 overview 透出用；与 /api/system/check-update
// 的 current 同一来源）。
func AppVersion() string { return appVersion }

// versionCheckState 检查结果缓存（6 小时：与 manager 口径一致，避免每次进面板都打 GitHub）。
type versionCheckState struct {
	mu        sync.Mutex
	fetched   time.Time
	snapshot  UpdateCheck
	lastError string
}

var versionCheck versionCheckState

// UpdateCheck 版本检查响应（面板 settings 页「版本检查提示」契约）。
type UpdateCheck struct {
	Current      string `json:"current"`        // 当前版本（dev = 开发版）
	Latest       string `json:"latest"`         // 上游最新 release tag
	HasUpdate    bool   `json:"has_update"`     // 语义化比较 current < latest
	ChangelogURL string `json:"changelog_url"`  // 上游 release 页链接
	Cached       bool   `json:"cached"`         // 命中缓存（force=false 且未过期）
	CheckedAt    string `json:"checked_at,omitempty"` // 本次/上次检查时间
}

// upstreamReleaseAPI workbuddy-cockpit 仓库自身的 release 查询端点。
// 注意：设计文档 §4.3.3 说「比对 GitHub 上游 release」——本 fork 即对外发行主体，
// 故比对自己的 latest release（fork 后版本号自管，v1.2.0 起步），而非上游 Sliverkiss。
const upstreamReleaseAPI = "https://api.github.com/repos/Arimayuki03/workbuddy-cockpit/releases/latest"

// versionCacheTTL 检查结果缓存时长。
const versionCacheTTL = 6 * time.Hour

// versionHTTPClient GitHub API 客户端超时（独立于上游 client；只读、低频）。
var versionHTTPClient = &http.Client{Timeout: 15 * time.Second}

// versionRe 语义化版本 tag（v 前缀 + 数字点分；取主前缀比较，预发布后缀忽略）。
var versionRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)`)

// handleCheckUpdate GET /api/system/check-update?force=true
// 只读端点：查 GitHub latest release 与当前版本比较，**不做自更新**（设计文档 §5）。
// force=true 绕过缓存；失败回退缓存值（有则用），无缓存时返回 current+错误说明而非 500——
// 网络受限（无外网部署）是常态而非异常，面板应照常可用。
func (h *Handler) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"

	versionCheck.mu.Lock()
	cached, cacheTime, lastErr := versionCheck.snapshot, versionCheck.fetched, versionCheck.lastError
	versionCheck.mu.Unlock()

	if !force && cacheTime.Add(versionCacheTTL).After(time.Now()) {
		cached.Cached = true
		cached.CheckedAt = cacheTime.Format(time.RFC3339)
		writeJSON(w, http.StatusOK, cached)
		return
	}

	latest, err := fetchLatestRelease(r.Context())
	if err != nil {
		// 无网/限流：有缓存回退缓存（标注 cached），无缓存回退 Unknown——
		// 不 500：版本检查是提示性功能，网络失败不应打断面板可用性。
		if cacheTime.IsZero() {
			writeJSON(w, http.StatusOK, UpdateCheck{
				Current:      appVersion,
				Latest:       "unknown",
				ChangelogURL: "https://github.com/Arimayuki03/workbuddy-cockpit/releases",
			})
			return
		}
		_ = lastErr // 上次成功的结果已回退；错误只进日志（低频路径，WARN 一行足够）
		log.Printf("WARN: [server] check-update: %v（回退缓存值 %s）", err, cached.Latest)
		cached.Cached = true
		cached.CheckedAt = cacheTime.Format(time.RFC3339)
		writeJSON(w, http.StatusOK, cached)
		return
	}

	snap := UpdateCheck{
		Current:      appVersion,
		Latest:       latest,
		HasUpdate:    versionLess(appVersion, latest),
		ChangelogURL: "https://github.com/Arimayuki03/workbuddy-cockpit/releases/latest",
	}
	versionCheck.mu.Lock()
	versionCheck.snapshot, versionCheck.fetched, versionCheck.lastError = snap, time.Now(), ""
	versionCheck.mu.Unlock()
	writeJSON(w, http.StatusOK, snap)
}

// fetchLatestRelease 拉取 GitHub latest release tag。
func fetchLatestRelease(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamReleaseAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("WB2A_GITHUB_TOKEN"); tok != "" {
		// 可选令牌：无外网限制/限流场景提高限额。未设置则匿名（60 req/h 足够低频检查）。
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := versionHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errReleaseStatus(resp.StatusCode)
	}
	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.TagName == "" {
		return "", errReleaseEmpty
	}
	return out.TagName, nil
}

type errReleaseStatus int

func (e errReleaseStatus) Error() string {
	return fmt.Sprintf("github api: %s", http.StatusText(int(e)))
}

var errReleaseEmpty = fmt.Errorf("release tag_name empty")

// versionLess 语义化版本比较：a < b 时 true。预发布后缀（-rc1 等）忽略；
// 非法形态（含 dev）→ false（开发版/未知版本不提示更新）。
func versionLess(a, b string) bool {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return false
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			return av[i] < bv[i]
		}
	}
	return false
}

// parseVersion 提取 v主.次.补（预发布后缀忽略）。
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return out, false
	}
	out[0], _ = strconv.Atoi(m[1])
	out[1], _ = strconv.Atoi(m[2])
	out[2], _ = strconv.Atoi(m[3])
	return out, true
}
