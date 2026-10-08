// profile_test.go 钉住账号资料拉取（panel f1496d0a 移植）：Web 形态请求头 +
// /console/account 路径；只解析 nickname（敏感字段不得透出）；uid 不一致防串号；
// 业务错误经 doJSON 信封原样返回 *Error。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestFetchAccountProfile(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/console/account") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer at" {
			return nil, errors.New("missing bearer")
		}
		if r.Header.Get("x-client-platform") != "web" {
			return nil, errors.New("missing web platform header")
		}
		if r.Header.Get("X-User-Id") != "u1" {
			return nil, errors.New("missing X-User-Id")
		}
		// 响应刻意带 phoneNumber：方法必须只解析 nickname/uid，敏感字段不得进入返回值。
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"uid":"u1","nickname":"新名字","phoneNumber":"13800000000"}}`), nil
	})
	nick, err := c.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil || nick != "新名字" {
		t.Fatalf("nick=%q err=%v, want 新名字 nil", nick, err)
	}
}

func TestFetchAccountProfileGlobalRealmUsesGlobalBase(t *testing.T) {
	// realm 感知：global 账号走国际站 base（webBase 恒 defaultGlobalBase，WebBaseCN
	// 只管 CN），global 账号的 /console/account 不得打向 CN 官网域。
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(r.URL.String(), "https://www.workbuddy.ai") {
			return nil, errors.New("wrong web base: " + r.URL.String())
		}
		return jsonResp(200, `{"code":0,"data":{"uid":"u1","nickname":"g"}}`), nil
	})
	c.GlobalEnabled = true
	c.WebBaseCN = "https://web-cn.example"
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.workbuddy.ai"}
	nick, err := c.FetchAccountProfile(a)
	if err != nil || nick != "g" {
		t.Fatalf("nick=%q err=%v, want g nil", nick, err)
	}
}

func TestFetchAccountProfileUIDMismatch(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"uid":"someone-else","nickname":"x"}}`), nil
	})
	if _, err := c.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "at"}); err == nil {
		t.Fatal("uid 不一致应报错（防串号）")
	}
}

func TestFetchAccountProfileBusinessError(t *testing.T) {
	// 401 走 doJSON 的 HTTP >=400 分支，返回带分类的 *Error（调用方静默跳过）。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, `{"code":1002,"msg":"unauthorized"}`), nil
	})
	_, err := c.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "bad"})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("401 应返回 *Error，got %v (%T)", err, err)
	}
	if ue.Status != 401 {
		t.Fatalf("Status=%d, want 401", ue.Status)
	}
}

func TestFetchAccountProfileEmptyUIDSkipsGuard(t *testing.T) {
	// 老档案缺 uid 时不做防串号比对（双方都非空才比对），昵称照常返回。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"uid":"someone-else","nickname":"老档案"}}`), nil
	})
	nick, err := c.FetchAccountProfile(&auth.Auth{AccessToken: "at"})
	if err != nil || nick != "老档案" {
		t.Fatalf("nick=%q err=%v, want 老档案 nil", nick, err)
	}
}

// 防回归补充：解析层绝不透出敏感字段——响应里塞 phoneNumber，断言结果字符串
// 不含它（FetchAccountProfile 只返回 nickname，结构上不可能，但把边界钉进测试）。
func TestFetchAccountProfileDropsSensitiveFields(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"uid":"u1","nickname":"nick","phoneNumber":"13800000000","email":"a@b.c"}}`), nil
	})
	nick, err := c.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(nick, "13800000000") || strings.Contains(nick, "a@b.c") {
		t.Fatalf("敏感字段泄漏进返回值：%q", nick)
	}
}
