package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"shellgo/internal/audit"
	"shellgo/internal/auth"
	"shellgo/internal/config"
	"shellgo/internal/files"
	"shellgo/internal/server"
	"shellgo/internal/terminal"
)

//go:embed all:web
var webAssets embed.FS

var (
	buildVersion = "dev" // 由 build.sh 通过 -ldflags 注入真实版本
	buildCommit  = "dev"
	buildTime    = "unknown"
)

func main() {
	var (
		cfgPath   = flag.String("config", "", "配置文件路径（默认 ./config.yaml）")
		listen    = flag.String("listen", "", "监听地址，如 0.0.0.0:8443")
		userFlag  = flag.String("user", "", "登录用户名（覆盖配置文件）")
		passFlag  = flag.String("pass", "", "登录密码（覆盖配置文件，建议改用环境变量 SHELLGO_PASSWORD）")
		hashFlag  = flag.String("pass-hash", "", "bcrypt 密码哈希（优先于 -pass）")
		initCfg   = flag.Bool("init-config", false, "生成示例配置文件后退出")
		genHash   = flag.String("gen-hash", "", "生成 bcrypt 哈希后退出")
		showVer   = flag.Bool("version", false, "显示版本信息")
		allowRoot = flag.Bool("allow-root", false, "允许以 root 身份运行（默认禁止）")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("shell-go %s (commit %s, built %s)\n", buildVersion, buildCommit, buildTime)
		return
	}
	if *genHash != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(*genHash), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintln(os.Stderr, "生成失败：", err)
			os.Exit(1)
		}
		fmt.Println(string(h))
		return
	}
	if *initCfg {
		if err := writeExampleConfig(); err != nil {
			fmt.Fprintln(os.Stderr, "写入配置失败：", err)
			os.Exit(1)
		}
		fmt.Println("已生成 config.example.yaml")
		return
	}

	path := *cfgPath
	if path == "" {
		if env := os.Getenv("SHELLGO_CONFIG"); env != "" {
			path = env
		} else if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		fatal(err.Error())
	}

	// 命令行/环境变量覆盖账号
	user := *userFlag
	pass := *passFlag
	if user == "" {
		user = os.Getenv("SHELLGO_USER")
	}
	if pass == "" {
		pass = os.Getenv("SHELLGO_PASSWORD")
	}
	if user != "" {
		if *hashFlag != "" {
			cfg.Users = []config.User{{Username: user, PasswordHash: *hashFlag}}
		} else {
			cfg.Users = []config.User{{Username: user, Password: pass}}
		}
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *allowRoot {
		cfg.Security.AllowRoot = true
	}
	if err := cfg.Validate(); err != nil {
		fatal(err.Error())
	}
	if os.Geteuid() == 0 && !cfg.Security.AllowRoot {
		fatal("检测到以 root 身份运行，默认已拒绝。\n" +
			"  本工具会把执行任意命令的 shell 暴露在网页上，以 root 运行等于把整机交给网页账号。\n" +
			"  请改用普通用户运行；确有必要时显式追加 -allow-root 参数。")
	}

	logger, closer, err := setupLogger(cfg.Log)
	if err != nil {
		fatal(err.Error())
	}
	defer closer()

	am, err := auth.NewManager(cfg)
	if err != nil {
		fatal(err.Error())
	}
	al := audit.New(cfg.Audit.LogFile, cfg.Audit.Enabled)
	defer al.Close()
	tm := terminal.NewManager(&cfg.Terminal, cfg.Record, cfg.Security.MaxTerms)
	if cfg.Record.Enabled {
		terminal.CleanRecords(cfg.Record.Dir, cfg.Record.KeepDays, cfg.Record.MaxFiles)
	}
	fm, err := files.New(cfg.Files)
	if err != nil {
		fatal(err.Error())
	}

	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		fatal("前端资源加载失败：" + err.Error())
	}
	server.Version = fmt.Sprintf("%s (%s)", buildVersion, buildCommit)
	srv, err := server.New(cfg, am, tm, fm, al, logger, sub)
	if err != nil {
		fatal(err.Error())
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	// 清理协程
	stopSweep := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				am.Sweep()
				tm.Sweep()
				srv.Sweep()
			case <-stopSweep:
				return
			}
		}
	}()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fatal(fmt.Sprintf("监听 %s 失败：%v", cfg.Listen, err))
	}

	useTLS := cfg.TLS.Enabled
	scheme := "http"
	if useTLS {
		scheme = "https"
		var cert tls.Certificate
		if cfg.TLS.Cert != "" && cfg.TLS.Key != "" {
			cert, err = tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
			if err != nil {
				fatal("加载证书失败：" + err.Error())
			}
		} else {
			hosts := []string{"localhost", "127.0.0.1"}
			if h, _, err := net.SplitHostPort(cfg.Listen); err == nil && h != "0.0.0.0" && h != "::" {
				hosts = append(hosts, h)
			}
			if hn, err := os.Hostname(); err == nil {
				hosts = append(hosts, hn)
			}
			cert, err = selfSignedCert(hosts)
			if err != nil {
				fatal(err.Error())
			}
			logger.Warn("已启用自签证书，浏览器首次访问需手动信任")
		}
		httpSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}

	banner(cfg, scheme, logger, tm, am)

	errCh := make(chan error, 1)
	go func() {
		if useTLS {
			errCh <- httpSrv.ServeTLS(ln, "", "")
		} else {
			errCh <- httpSrv.Serve(ln)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sg := <-sig:
		logger.Info("收到退出信号，正在关闭", "signal", sg.String())
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务异常退出", "err", err)
		}
	}

	close(stopSweep)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	tm.CloseAll()
	logger.Info("已退出")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "[错误] "+msg)
	os.Exit(1)
}

func setupLogger(cfg config.Log) (*slog.Logger, func(), error) {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	writers := []io.Writer{os.Stdout}
	var f *os.File
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0o755); err == nil {
			var err error
			f, err = os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				writers = append(writers, f)
			}
		}
	}
	h := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: level})
	return slog.New(h), func() {
		if f != nil {
			_ = f.Close()
		}
	}, nil
}

func banner(cfg *config.Config, scheme string, log *slog.Logger, tm *terminal.Manager, am *auth.Manager) {
	host, port, _ := net.SplitHostPort(cfg.Listen)
	shown := host
	if host == "0.0.0.0" || host == "::" || host == "" {
		shown = localIP()
	}
	log.Info("shell-go 已启动",
		"version", buildVersion,
		"url", fmt.Sprintf("%s://%s:%s", scheme, shown, port),
		"listen", cfg.Listen,
		"transport", cfg.Terminal.Transport,
		"users", len(cfg.Users),
		"tls", scheme == "https",
		"audit", cfg.Audit.Enabled,
	)
	if len(cfg.Security.IPWhitelist) == 0 {
		log.Warn("未配置 IP 白名单，服务对所有可达地址开放，建议在 config.yaml 的 security.ip_whitelist 中限制来源")
	}
}

func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}

func writeExampleConfig() error {
	b, err := os.ReadFile("config.example.yaml")
	if err != nil {
		return err
	}
	return os.WriteFile("config.yaml", b, 0o600)
}
