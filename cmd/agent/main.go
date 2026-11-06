package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fulcrumproject/agent-lib-go/pkg/fulcrumcli"
	"github.com/fulcrumproject/agent-lib-go/pkg/stdagent"
	"github.com/fulcrumproject/agent-mysql/internal/config"
	"github.com/fulcrumproject/agent-mysql/internal/dockerapi"
	"github.com/fulcrumproject/agent-mysql/internal/mysqlagent"
	"github.com/fulcrumproject/utils/confbuilder"
	"github.com/fulcrumproject/utils/logging"
)

func main() {
	configPath := flag.String("config", "", "Path to configuration file")
	flag.Parse()
	cfg, err := confbuilder.New(config.DefaultConfig()).EnvPrefix(config.EnvPrefix).EnvFiles(".env").File(configPath).Build()
	if err != nil {
		slog.Error("Invalid configuration")
		os.Exit(1)
	}
	slog.SetDefault(logging.NewLogger(&cfg.LogConfig))
	if cfg.JobPollInterval <= 0 || cfg.MetricReportInterval <= 0 {
		slog.Error("Polling intervals must be positive")
		os.Exit(1)
	}
	core := fulcrumcli.NewHTTPClient(cfg.FulcrumAPIURL, cfg.AgentToken)
	a, err := mysqlagent.NewMySQLAgent(dockerapi.NewClient, core,
		stdagent.WithJobPollInterval(cfg.JobPollInterval), stdagent.WithMetricsReportInterval(cfg.MetricReportInterval),
		stdagent.WithHealthInterval(10*time.Second), stdagent.WithConnectRetryInterval(10*time.Second))
	if err != nil {
		slog.Error("Cannot construct agent", "error", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err = a.Run(ctx); err != nil {
		slog.Error("Cannot start agent", "error", err)
		os.Exit(1)
	}
	slog.Info("MySQL agent started", "agentID", a.GetAgentID())
	<-ctx.Done()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if err = a.Shutdown(shutdownCtx); err != nil {
		slog.Error("Shutdown failed", "error", err)
		os.Exit(1)
	}
}
