// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package stream

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tvhub/internal/store"
)

const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) TvHub/1.0"

// HTTPProxy 把 http(s) 源原样转发给浏览器（不解码、不缓存）。
type HTTPProxy struct {
	client *http.Client
	secret []byte
}

// NewHTTPProxy 创建反向代理，secret 用于给子地址签名。
func NewHTTPProxy(secret []byte) *HTTPProxy {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		ResponseHeaderTimeout: 25 * time.Second,
	}
	return &HTTPProxy{
		client: &http.Client{Transport: tr},
		secret: secret,
	}
}

// Sign 为绝对地址生成签名，防止用户把代理当成任意地址跳板。
func (p *HTTPProxy) Sign(u string) string {
	m := hmac.New(sha256.New, p.secret)
	m.Write([]byte(u))
	return hex.EncodeToString(m.Sum(nil))[:20]
}

func (p *HTTPProxy) encode(u string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(u))
}

// SubURL 生成带签名的子地址（供 m3u8 分片使用），extra 为附加查询串。
func (p *HTTPProxy) SubURL(channelID int64, u, extra string) string {
	q := "u=" + p.encode(u) + "&sig=" + p.Sign(u)
	if extra != "" {
		q += "&" + extra
	}
	return fmt.Sprintf("/proxy/%d/sub?%s", channelID, q)
}

// Serve 转发一个 http(s) 频道。extra 是附加查询串（通常是外部播放器用的 token）。
func (p *HTTPProxy) Serve(w http.ResponseWriter, r *http.Request, ch store.Channel, extra string) {
	resp, err := p.fetch(r.Context(), ch.URL, ch.Headers, r.Header.Get("Range"))
	if err != nil {
		http.Error(w, "拉取源站失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if isPlaylist(resp, ch.URL) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			http.Error(w, "读取播放列表失败: "+err.Error(), http.StatusBadGateway)
			return
		}
		base, err := url.Parse(ch.URL)
		if err != nil {
			http.Error(w, "源地址非法", http.StatusBadGateway)
			return
		}
		out := rewritePlaylist(string(body), base, func(abs string) string { return p.SubURL(ch.ID, abs, extra) })
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, out)
		return
	}

	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

// ServeSub 转发播放列表里引用的分片/密钥地址（必须是本服务签发的地址）。
func (p *HTTPProxy) ServeSub(w http.ResponseWriter, r *http.Request, ch store.Channel, extra string) {
	enc := r.URL.Query().Get("u")
	sig := r.URL.Query().Get("sig")
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		http.Error(w, "地址参数非法", http.StatusBadRequest)
		return
	}
	target := string(raw)
	if !hmac.Equal([]byte(p.Sign(target)), []byte(sig)) {
		http.Error(w, "签名校验失败", http.StatusForbidden)
		return
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		http.Error(w, "只支持 http(s) 地址", http.StatusBadRequest)
		return
	}
	resp, err := p.fetch(r.Context(), target, ch.Headers, r.Header.Get("Range"))
	if err != nil {
		http.Error(w, "拉取源站失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if isPlaylist(resp, target) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		base, _ := url.Parse(target)
		out := rewritePlaylist(string(body), base, func(abs string) string { return p.SubURL(ch.ID, abs, extra) })
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, out)
		return
	}
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

func (p *HTTPProxy) fetch(ctx context.Context, target, headerBlock, rng string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUA)
	for _, line := range strings.Split(headerBlock, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	return p.client.Do(req)
}

func isPlaylist(resp *http.Response, u string) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "mpegurl") {
		return true
	}
	low := strings.ToLower(u)
	if i := strings.IndexAny(low, "?#"); i >= 0 {
		low = low[:i]
	}
	return strings.HasSuffix(low, ".m3u8") || strings.HasSuffix(low, ".m3u")
}

// rewritePlaylist 把播放列表里的媒体地址改写成经本服务转发的地址。
func rewritePlaylist(text string, base *url.URL, fn func(string) string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		ref, err := url.Parse(t)
		if err != nil {
			continue
		}
		lines[i] = fn(base.ResolveReference(ref).String())
	}
	return strings.Join(lines, "\n")
}

func copyUpstreamHeaders(dst, src http.Header) {
	for _, k := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range", "Last-Modified", "ETag"} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/octet-stream")
	}
}

// flushCopy 边读边发，保证直播流低延迟。
func flushCopy(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
