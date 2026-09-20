// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

// 命令 tvhub：把只有内网可达的直播源（RTSP/RTMP/UDP/HTTP）转成浏览器可播的
// HLS/HTTP 流，自带用户管理与 M3U 播放列表管理。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tvhub/internal/auth"
	"tvhub/internal/config"
	"tvhub/internal/m3u"
	"tvhub/internal/store"
	"tvhub/internal/stream"
	"tvhub/internal/webui"
)

const version = "1.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr       = flag.String("addr", envOr("TVHUB_ADDR", ":8099"), "HTTP 监听地址")
		dataDir    = flag.String("data", envOr("TVHUB_DATA", "data"), "数据目录（SQLite 与 HLS 分片）")
		adminUser  = flag.String("admin-user", envOr("TVHUB_ADMIN_USER", "admin"), "首次启动创建的管理员用户名")
		adminPass  = flag.String("admin-pass", envOr("TVHUB_ADMIN_PASSWORD", ""), "首次启动创建的管理员密码，留空则随机生成并打印")
		importFile = flag.String("import", "", "启动时导入该 m3u 文件（可作为频道的初始/增量来源）")
		baseURL    = flag.String("base-url", envOr("TVHUB_BASE_URL", ""), "对外访问地址，如 http://192.168.1.10:8099，用于生成外部播放链接")
		resetPW    = flag.Bool("reset-admin-password", false, "重置管理员密码后退出")
		setKV      = flag.String("set", "", "写入设置项后退出，格式 key=value，多项用逗号分隔（键名见「参数设置」页）")
		showVer    = flag.Bool("version", false, "显示版本后退出")
		debug      = flag.Bool("debug", false, "打印调试日志")
		ffmpegPath = flag.String("ffmpeg", envOr("TVHUB_FFMPEG", ""), "ffmpeg 路径（提供后会写入设置，命令行优先于面板）")
		tlsCert    = flag.String("tls-cert", envOr("TVHUB_TLS_CERT", ""), "HTTPS 证书文件（与 -tls-key 同时提供才启用）")
		tlsKey     = flag.String("tls-key", envOr("TVHUB_TLS_KEY", ""), "HTTPS 私钥文件")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("tvhub", version)
		return nil
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	st, err := store.Open(filepath.Join(*dataDir, "tvhub.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	am := auth.New(st)

	// ffmpeg 路径：命令行 > 随程序携带 > 面板设置
	switch {
	case strings.TrimSpace(*ffmpegPath) != "":
		if err := st.SetSetting(config.KeyFFmpegPath, *ffmpegPath); err != nil {
			return err
		}
		log.Info("使用命令行指定的 ffmpeg", "path", *ffmpegPath)
	case strings.TrimSpace(st.Setting(config.KeyFFmpegPath, "")) == "":
		if p := discoverFFmpeg(executableDir()); p != "" {
			if err := st.SetSetting(config.KeyFFmpegPath, p); err != nil {
				return err
			}
			log.Info("发现随程序携带的 ffmpeg，已自动填入设置", "path", p)
		}
	}

	if *resetPW {
		return resetAdminPassword(st, am, *adminUser)
	}

	if strings.TrimSpace(*setKV) != "" {
		return applySettings(st, *setKV)
	}

	// 首次启动：创建管理员
	if n, _ := st.CountUsers(); n == 0 {
		pw := *adminPass
		generated := false
		if strings.TrimSpace(pw) == "" {
			pw = auth.NewToken(6)
			generated = true
		}
		hash, err := am.Hash(pw)
		if err != nil {
			return err
		}
		id, err := st.CreateUser(*adminUser, hash, "admin")
		if err != nil {
			return fmt.Errorf("创建管理员失败: %w", err)
		}
		if err := st.SetUserStreamToken(id, auth.NewToken(16)); err != nil {
			return err
		}
		log.Info("已创建初始管理员", "user", *adminUser)
		if generated {
			fmt.Printf("\n================ 初始管理员账号 ================\n  用户名：%s\n  密  码：%s\n（登录后请在“用户”页修改密码）\n===============================================\n\n", *adminUser, pw)
		}
	}

	// 服务密钥：用于给代理子地址签名
	secret := st.Setting(config.KeySecret, "")
	if secret == "" {
		secret = auth.NewToken(32)
		if err := st.SetSetting(config.KeySecret, secret); err != nil {
			return err
		}
	}

	// 导入初始播放列表
	if strings.TrimSpace(*importFile) != "" {
		if err := importPlaylistFile(st, *importFile); err != nil {
			return err
		}
		log.Info("已导入播放列表", "file", *importFile)
	}

	optsFn := func() stream.Options { return config.Load(st).StreamOptions() }
	mgr := stream.NewManager(filepath.Join(*dataDir, "hls"), optsFn, log)
	mgr.Start()
	defer mgr.StopAll()

	proxy := stream.NewHTTPProxy([]byte(secret))
	web, err := webui.New(st, am, mgr, proxy, version, *baseURL, log)
	if err != nil {
		return err
	}

	// 定时清理过期会话
	stopCleanup := make(chan struct{})
	defer close(stopCleanup)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-stopCleanup:
				return
			case <-t.C:
				if n, err := st.PurgeExpiredSessions(); err == nil && n > 0 {
					log.Debug("清理过期会话", "count", n)
				}
			}
		}
	}()

	// ffmpeg 自检
	o := optsFn()
	if ver, err := o.FFmpegVersion(); err != nil {
		log.Warn("未检测到可用的 ffmpeg，无法转发 RTSP/RTMP/UDP 源", "path", o.FFmpeg, "err", err)
	} else {
		log.Info("ffmpeg 就绪", "version", ver)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           web.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	useTLS := *tlsCert != "" && *tlsKey != ""
	if useTLS {
		am.SetSecureCookie(true)
		go func() { errCh <- srv.ListenAndServeTLS(*tlsCert, *tlsKey) }()
	} else {
		go func() { errCh <- srv.ListenAndServe() }()
	}
	total, _, _ := st.CountChannels()
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	log.Info("服务已启动", "addr", *addr, "scheme", scheme, "频道数", total, "数据目录", *dataDir)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-sigCh:
		log.Info("收到退出信号，正在停止…")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	return nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// executableDir 返回可执行文件所在目录（用于查找随程序携带的 ffmpeg）。
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// discoverFFmpeg 查找与程序放在一起的 ffmpeg（ffmpeg/bin/、bin/、同目录）。
func discoverFFmpeg(dir string) string {
	var cands []string
	for _, name := range []string{"ffmpeg.exe", "ffmpeg"} {
		cands = append(cands,
			filepath.Join(dir, "ffmpeg", "bin", name),
			filepath.Join(dir, "bin", name),
			filepath.Join(dir, name),
		)
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

func importPlaylistFile(st *store.Store, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取播放列表失败: %w", err)
	}
	entries := m3u.ApplyGroups(m3u.Parse(string(b)))
	if len(entries) == 0 {
		return fmt.Errorf("播放列表 %s 中没有解析到频道", path)
	}
	res, err := st.ImportChannels(entries)
	if err != nil {
		return err
	}
	_ = res
	return st.SetSettings(map[string]string{
		config.KeyM3UContent: string(b),
		config.KeyM3USource:  "文件: " + filepath.Base(path),
		config.KeyM3UApplied: time.Now().Format("2006-01-02 15:04:05"),
	})
}

// applySettings 写入设置项后退出，便于脚本化运维（例如批量关掉某个开关）。
func applySettings(st *store.Store, spec string) error {
	types := map[string]string{}
	for _, f := range config.Fields() {
		types[f.Key] = f.Type
	}
	kv := map[string]string{}
	var shown []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return fmt.Errorf("设置项格式应为 key=value：%s", part)
		}
		typ, known := types[k]
		if !known {
			return fmt.Errorf("未知设置项 %q（键名见「参数设置」页）", k)
		}
		val = strings.TrimSpace(val)
		kv[k] = val
		shown = append(shown, k+"="+maskValue(typ, val))
	}
	if len(kv) == 0 {
		return errors.New("没有解析到任何设置项")
	}
	if err := st.SetSettings(kv); err != nil {
		return err
	}
	fmt.Println("已写入设置：" + strings.Join(shown, ", "))
	return nil
}

// maskValue 避免把密钥类设置项回显到终端/日志里。
func maskValue(typ, val string) string {
	if typ == "password" {
		if val == "" {
			return "(留空)"
		}
		return "***"
	}
	return val
}

func resetAdminPassword(st *store.Store, am *auth.Manager, adminUser string) error {
	u, err := st.UserByUsername(adminUser)
	if err != nil {
		list, lerr := st.ListUsers()
		if lerr != nil || len(list) == 0 {
			return errors.New("没有找到任何用户")
		}
		for i := range list {
			if list[i].IsAdmin() {
				u = &list[i]
				break
			}
		}
		if u == nil {
			return errors.New("没有找到管理员账号")
		}
	}
	pw := auth.NewToken(6)
	hash, err := am.Hash(pw)
	if err != nil {
		return err
	}
	if err := st.UpdateUserPassword(u.ID, hash); err != nil {
		return err
	}
	fmt.Printf("\n已重置管理员 %s 的密码：%s\n\n", u.Username, pw)
	return nil
}
