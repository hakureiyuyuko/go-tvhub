// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TurnstileVerifyURL 是 Cloudflare Turnstile 的服务端校验地址（测试时可替换）。
var TurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// TurnstileResult 是校验结果。
type TurnstileResult struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
	Hostname   string   `json:"hostname"`
}

// VerifyTurnstile 把一个客户端提交的 token 交给 Cloudflare 校验。
// 返回的 error 只在「请求本身失败」（网络/超时/响应异常）时非空，
// 校验不通过时返回 Success=false 且 err==nil。
func VerifyTurnstile(ctx context.Context, secret, token, remoteIP string) (*TurnstileResult, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("未配置 Turnstile 密钥")
	}
	if strings.TrimSpace(token) == "" {
		return &TurnstileResult{Success: false, ErrorCodes: []string{"missing-input-response"}}, nil
	}
	form := url.Values{}
	form.Set("secret", secret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TurnstileVerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Cloudflare 返回 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out TurnstileResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析 Cloudflare 响应失败: %w", err)
	}
	return &out, nil
}

// TurnstileErrorText 把错误码翻译成给人看的说明。
func TurnstileErrorText(codes []string) string {
	for _, c := range codes {
		switch c {
		case "missing-input-response":
			return "没有收到验证结果，请等待人机验证完成后再登录"
		case "invalid-input-response", "invalid-input-secret":
			return "验证无效，站点密钥或密钥配置有误"
		case "timeout-or-duplicate":
			return "验证已过期，请重新完成人机验证"
		case "bad-request":
			return "验证请求异常，请刷新页面重试"
		}
	}
	return "人机验证未通过，请重试"
}
