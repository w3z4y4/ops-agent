package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/yourorg/slb-ops-agent/internal/config"
	"github.com/yourorg/slb-ops-agent/internal/executor"
	"github.com/yourorg/slb-ops-agent/internal/filemanager"
	"github.com/yourorg/slb-ops-agent/internal/grpcserver"
	"github.com/yourorg/slb-ops-agent/internal/health"
	"github.com/yourorg/slb-ops-agent/internal/logger"
	"github.com/yourorg/slb-ops-agent/internal/servicectl"
	"github.com/yourorg/slb-ops-agent/internal/upgrade"
)

// Version 由编译时通过 -ldflags "-X main.Version=x.y.z" 注入。
var Version = "dev"

func main() {
	cfgPath := flag.String("config", "/etc/slb-agent/config.yaml", "path to config file")
	flag.Parse()

	// ── 加载配置 ─────────────────────────────────────────────────────────────
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: load config: %v\n", err)
		os.Exit(1)
	}
	cfg.Agent.Version = Version

	// ── 审计日志 ─────────────────────────────────────────────────────────────
	audit, err := logger.NewAuditLogger(cfg.Logging.AuditFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: init audit logger: %v\n", err)
		os.Exit(1)
	}
	defer audit.Close()

	// ── 子模块初始化 ──────────────────────────────────────────────────────────
	exec := executor.New()

	fileMgr := filemanager.New()

	svcCtl := servicectl.New(cfg.Services.AllowedServices)

	prober := health.New(health.Config{
		CheckInterval:      cfg.Health.CheckInterval,
		HaproxyConfigFile:  cfg.Health.HaproxyConfigFile,
		HaproxyStatsSocket: cfg.Health.HaproxyStatsSocket,
		NodeExporterPort:   cfg.Health.NodeExporterPort,
		ConfdPort:          cfg.Health.ConfdPort,
		AgentVersion:       Version,
	})
	defer prober.Stop()

	upgrader := upgrade.New(&cfg.Agent, &cfg.Upgrade, Version)

	// ── gRPC Handler & Server ────────────────────────────────────────────────
	handler := grpcserver.New(exec, fileMgr, svcCtl, prober, upgrader)

	srv, err := grpcserver.NewServer(cfg, handler, audit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: init grpc server: %v\n", err)
		os.Exit(1)
	}

	// ── 信号处理（SIGTERM / SIGINT 优雅退出）────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// 在独立 goroutine 中启动 gRPC 监听
	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("slb-ops-agent %s listening on %s\n", Version, cfg.GRPC.ListenAddr)
		errCh <- srv.Serve()
	}()

	// 阻塞，等待信号或服务器错误
	select {
	case sig := <-sigCh:
		fmt.Printf("received signal %v, shutting down gracefully...\n", sig)
		srv.GracefulStop()

	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: grpc serve: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("slb-ops-agent exited cleanly.")
}
