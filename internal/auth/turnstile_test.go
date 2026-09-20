// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withVerifyServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	old := TurnstileVerifyURL
	TurnstileVerifyURL = srv.URL
	t.Cleanup(func() {
		TurnstileVerifyURL = old
		srv.Close()
	})
	return srv
}

func TestVerifyTurnstileSuccess(t *testing.T) {
	var gotSecret, gotToken, gotIP string
	withVerifyServer(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotSecret, gotToken, gotIP = r.PostFormValue("secret"), r.PostFormValue("response"), r.PostFormValue("remoteip")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"hostname":"tv.local","challenge_ts":"2026-01-01T00:00:00Z"}`))
	})
	res, err := VerifyTurnstile(context.Background(), "s3cret", "tok123", "10.0.0.9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("want success, got %+v", res)
	}
	if gotSecret != "s3cret" || gotToken != "tok123" || gotIP != "10.0.0.9" {
		t.Fatalf("参数传递错误: secret=%q token=%q ip=%q", gotSecret, gotToken, gotIP)
	}
}

func TestVerifyTurnstileFailure(t *testing.T) {
	withVerifyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	})
	res, err := VerifyTurnstile(context.Background(), "s3cret", "bad", "")
	if err != nil {
		t.Fatalf("校验不通过不该返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("want failure")
	}
	if msg := TurnstileErrorText(res.ErrorCodes); !strings.Contains(msg, "验证无效") {
		t.Fatalf("错误提示不友好: %s", msg)
	}
}

func TestVerifyTurnstileMissingToken(t *testing.T) {
	withVerifyServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("token 为空时不应发起请求")
	})
	res, err := VerifyTurnstile(context.Background(), "s3cret", "  ", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success || len(res.ErrorCodes) == 0 {
		t.Fatalf("空 token 应直接判定失败: %+v", res)
	}
	if !strings.Contains(TurnstileErrorText(res.ErrorCodes), "人机验证") {
		t.Fatalf("提示不友好: %v", res.ErrorCodes)
	}
}

func TestVerifyTurnstileNetworkError(t *testing.T) {
	withVerifyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	})
	if _, err := VerifyTurnstile(context.Background(), "s3cret", "tok", ""); err == nil {
		t.Fatal("HTTP 500 时应返回 error（调用方据此决定放行）")
	}
	if _, err := VerifyTurnstile(context.Background(), "", "tok", ""); err == nil {
		t.Fatal("未配置密钥时应返回 error")
	}
}
