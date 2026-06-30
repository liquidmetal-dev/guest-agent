// Package agent implements the guest-side daemon: it accepts host connections
// over a transport (vsock in production), runs commands on the control port,
// and proxies the ssh port to the local sshd.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/liquidmetal-dev/guest-agent/internal/transport"
)

// Config configures an Agent.
type Config struct {
	Transport   transport.Config
	ControlPort uint32
	SSHPort     uint32
	// SSHTarget is the local address the ssh proxy dials, e.g. 127.0.0.1:22.
	SSHTarget string
	Logger    *slog.Logger
}

// Agent owns the listeners and the goroutines servicing them.
type Agent struct {
	cfg Config
	log *slog.Logger

	wg sync.WaitGroup
}

// New builds an Agent. A nil Logger defaults to slog.Default.
func New(cfg Config) *Agent {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Agent{cfg: cfg, log: cfg.Logger}
}

// Run opens both listeners and serves until ctx is cancelled, then closes the
// listeners and waits for in-flight connections to drain.
func (a *Agent) Run(ctx context.Context) error {
	control, err := transport.Listen(a.cfg.Transport, a.cfg.ControlPort)
	if err != nil {
		return err
	}
	ssh, err := transport.Listen(a.cfg.Transport, a.cfg.SSHPort)
	if err != nil {
		control.Close()
		return err
	}

	a.log.Info("guest-agent listening",
		"transport", a.cfg.Transport.Kind,
		"control_port", a.cfg.ControlPort,
		"ssh_port", a.cfg.SSHPort,
		"ssh_target", a.cfg.SSHTarget,
	)

	a.wg.Add(2)
	go a.serve(ctx, control, "control", a.handleControl)
	go a.serve(ctx, ssh, "ssh", a.handleSSH)

	// Close listeners on shutdown to unblock the Accept loops.
	go func() {
		<-ctx.Done()
		control.Close()
		ssh.Close()
	}()

	a.wg.Wait()
	return nil
}

// serve runs an Accept loop, dispatching each connection to handler in its own
// goroutine. It returns when the listener is closed (shutdown).
func (a *Agent) serve(ctx context.Context, l net.Listener, name string, handler func(context.Context, net.Conn)) {
	defer a.wg.Done()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return // graceful shutdown
			}
			a.log.Warn("accept failed", "listener", name, "error", err)
			continue
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			defer conn.Close()
			handler(ctx, conn)
		}()
	}
}
