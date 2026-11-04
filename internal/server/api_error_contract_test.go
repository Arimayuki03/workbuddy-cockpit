package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decodeGatewayError(t *testing.T, rec *httptest.ResponseRecorder) (code, typ string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", ct)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 错误体损坏: %v body=%s", err, rec.Body.String())
	}
	return body.Error.Code, body.Error.Type
}

func TestGatewayUnknownPathReturnsJSON404(t *testing.T) {
	h := NewHandler(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	code, typ := decodeGatewayError(t, rec)
	if code != "not_found" || typ != "invalid_request_error" {
		t.Fatalf("error=%s/%s want not_found/invalid_request_error", code, typ)
	}
}

func TestGatewayMethodMismatchReturnsJSON405(t *testing.T) {
	h := NewHandler(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405 body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Allow") == "" {
		t.Fatal("405 必须保留 Allow 头")
	}
	code, typ := decodeGatewayError(t, rec)
	if code != "method_not_allowed" || typ != "invalid_request_error" {
		t.Fatalf("error=%s/%s want method_not_allowed/invalid_request_error", code, typ)
	}
}

func TestGatewayPathVariantStillJSON(t *testing.T) {
	h := NewHandler(Config{})
	for _, path := range []string{"/v2", "/v2/models"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d want 404", path, rec.Code)
		}
		if code, _ := decodeGatewayError(t, rec); code != "not_found" {
			t.Fatalf("%s code=%s", path, code)
		}
	}
}
