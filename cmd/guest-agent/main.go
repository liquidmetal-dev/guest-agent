// Command guest-agent is the in-VM daemon. It accepts host connections over
// vsock (or TCP in dev mode), runs commands on the control port, and proxies
// the ssh port to the local sshd.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/liquidmetal-dev/guest-agent/internal/agent"
	"github.com/liquidmetal-dev/guest-agent/internal/transport"
)

func main() {
	var (
		net         = flag.String("net", envStr("GA_NET", "vsock"), "transport: vsock|tcp")
		controlPort = flag.Uint("control-port", envUint("GA_CONTROL_PORT", 1024), "vsock/tcp control port")
		sshPort     = flag.Uint("ssh-port", envUint("GA_SSH_PORT", 1025), "vsock/tcp ssh proxy port")
		sshTarget   = flag.String("ssh-target", envStr("GA_SSH_TARGET", "127.0.0.1:22"), "local sshd address")
		tcpAddr     = flag.String("tcp-addr", envStr("GA_TCP_ADDR", "127.0.0.1"), "bind host for --net tcp (dev)")
		logLevel    = flag.String("log-level", envStr("GA_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
		logFormat   = flag.String("log-format", envStr("GA_LOG_FORMAT", "text"), "log format: text|json")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(agent.Version)
		return
	}

	logger := newLogger(*logLevel, *logFormat)
	slog.SetDefault(logger)

	cfg := agent.Config{
		Transport:   transport.Config{Kind: transport.Kind(*net), Addr: *tcpAddr},
		ControlPort: uint32(*controlPort),
		SSHPort:     uint32(*sshPort),
		SSHTarget:   *sshTarget,
		Logger:      logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := agent.New(cfg).Run(ctx); err != nil {
		logger.Error("agent stopped with error", "error", err)
		os.Exit(1)
	}
	logger.Info("agent shut down")
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envUint(key string, def uint) uint {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			return uint(n)
		}
	}
	return def
}
