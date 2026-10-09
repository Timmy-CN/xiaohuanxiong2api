// Command xhx2api 是小浣熊（商汤 Raccoon）OpenAI 兼容反代的入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xhx2api/internal/checkin"
	"xhx2api/internal/config"
	"xhx2api/internal/logx"
	"xhx2api/internal/pool"
	"xhx2api/internal/server"
	"xhx2api/internal/upstream"
	"xhx2api/internal/version"
)

func main() {
	configPath := flag.String("config", "./config.json", "配置文件路径（JSONC）")
	healthcheckURL := flag.String("healthcheck", "", "健康检查模式：对给定 URL 发一次 GET，2xx 退出码 0，否则 1（不启动服务）")
	flag.Parse()

	// 健康检查模式：只发一次 GET，绝不起服务（SPEC2 §C.1）。
	if *healthcheckURL != "" {
		if err := runHealthcheck(*healthcheckURL); err != nil {
			fmt.Fprintf(os.Stderr, "健康检查失败：%v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	cfg, err := config.Load(config.ResolvePath(*configPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败：%v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg)

	client := upstream.NewClient(upstream.Options{
		LLMBase:               cfg.Upstream.LLMBaseURL(),
		AuthBase:              cfg.Upstream.AuthBaseURL(),
		PointsBase:            cfg.Upstream.PointsBaseURL(),
		Origin:                cfg.Upstream.OriginURL(),
		ConnectTimeout:        cfg.Upstream.ConnectTimeout(),
		ResponseHeaderTimeout: cfg.Upstream.ResponseHeaderTimeout(),
		RequestTimeout:        cfg.Upstream.RequestTimeout(),
		RefreshTimeout:        cfg.Upstream.RefreshTimeout(),
		CatalogTimeout:        cfg.Upstream.CatalogTimeout(),
		BalanceTimeout:        cfg.Upstream.BalanceTimeout(),
		BillsTimeout:          cfg.Upstream.BillsTimeout(),
		InsecureSkipVerify:    cfg.Upstream.SkipVerify(),
		UserAgent:             cfg.Upstream.UserAgentValue(),
		ExtraHeaders:          cfg.Upstream.ExtraHeaders,
	})

	accountPool := pool.New(pool.Config{
		File:                 cfg.Accounts.AccountsFile(),
		Strategy:             cfg.Accounts.StrategyValue(),
		RefreshMarginSeconds: int(cfg.Accounts.RefreshMargin().Seconds()),
		CooldownSeconds:      int(cfg.Accounts.Cooldown().Seconds()),
		MaxAttempts:          cfg.Accounts.MaxAttemptsValue(),
		RefreshOn401:         cfg.Accounts.RefreshOn401Value(),
	}, client, logger)

	catalog := server.NewCatalog(client, cfg.Models.CatalogCacheTTL(), cfg.Models.CatalogCacheFileValue(), cfg.Models.Fallback)
	catalog.LoadCache()
	if err := catalog.Refresh(context.Background(), accountPool.FirstToken()); err != nil {
		logger.Warnf("[catalog] 远程目录拉取失败（沿用 %s）：%v", catalog.Source(), err)
	}

	checkinSvc := checkin.New(checkin.Config{
		Enabled:           cfg.Checkin.EnabledValue(),
		Time:              cfg.Checkin.TimeValue(),
		OnStart:           cfg.Checkin.OnStartValue(),
		VerifyBills:       cfg.Checkin.VerifyBillsValue(),
		BillsLimit:        cfg.Checkin.BillsLimitValue(),
		RetryInterval:     cfg.Checkin.RetryInterval(),
		MaxAttemptsPerDay: cfg.Checkin.MaxAttemptsPerDayValue(),
		HistoryFile:       cfg.Checkin.HistoryFileValue(),
		HistoryDays:       cfg.Checkin.HistoryDaysValue(),
	}, client, accountPool, logger)

	printStartup(cfg, accountPool, catalog, logger)

	app := server.New(cfg, client, accountPool, catalog, logger, version.Version)
	app.SetCheckin(checkinSvc)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败：监听 %s 失败：%v\n", cfg.ListenAddr(), err)
		os.Exit(3)
	}

	if cfg.Checkin.EnabledValue() {
		checkinSvc.Start()
		defer checkinSvc.Stop()
	}

	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "服务异常退出：%v\n", err)
			os.Exit(3)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Infof("[main] 收到退出信号，开始优雅退出（超时 %s）", cfg.ShutdownTimeout())

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout())
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Warnf("[main] 优雅退出失败：%v", err)
	}
	logger.Infof("[main] 已退出")
}

// runHealthcheck 对 URL 发一次 GET：2xx → nil，否则返回错误。
func runHealthcheck(url string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// newLogger 构造分级日志器：始终写 stdout，可选再写文件。
func newLogger(cfg *config.Config) *logx.Logger {
	level, err := logx.ParseLevel(cfg.Log.LogLevel())
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告：%v，回退 info\n", err)
		level = logx.LevelInfo
	}
	var writers []io.Writer
	writers = append(writers, os.Stdout)
	if file := cfg.Log.LogFile(); file != "" {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告：打开日志文件失败（%s）：%v\n", file, err)
		} else {
			writers = append(writers, f)
		}
	}
	return logx.New(io.MultiWriter(writers...), "", level)
}

// printStartup 打印生效配置与启动摘要（SPEC §3.3）。
//
// 这些行是运维现场唯一的启动证据，用 Alwaysf 无条件输出，
// 不受 log.level 阈值影响（SPEC3 §E1）；其余日志照旧受阈值控制。
func printStartup(cfg *config.Config, accountPool *pool.Pool, catalog *server.Catalog, logger *logx.Logger) {
	if masked, err := cfg.MaskedJSON(); err == nil {
		logger.Alwaysf("[main] 生效配置：\n%s", string(masked))
	}
	total, _, available, _ := accountPool.Counts()
	logger.Alwaysf("[main] 日志级别=%s", cfg.Log.LogLevel())
	logger.Alwaysf("[main] 版本=%s 监听=%s 账号=%d/%d 目录=%d（来源：%s）",
		version.Version, cfg.ListenAddr(), available, total, catalog.Count(), catalog.Source())
}
