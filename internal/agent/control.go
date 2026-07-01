package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/protocol"
)

// Version is the agent build version, overridable via -ldflags at build time.
var Version = "dev"

// startTime is captured at process start to report uptime in OpInfo.
var startTime = time.Now()

// handleControl reads the opening Request frame and dispatches by op.
func (a *Agent) handleControl(ctx context.Context, conn net.Conn) {
	f, err := protocol.ReadFrame(conn)
	if err != nil {
		a.log.Warn("control: read request frame", "error", err)
		return
	}
	if f.Type != protocol.FrameRequest {
		protocol.WriteError(conn, fmt.Sprintf("expected request frame, got %s", f.Type))
		protocol.WriteExit(conn, 1)
		return
	}

	var req protocol.Request
	if err := json.Unmarshal(f.Payload, &req); err != nil {
		protocol.WriteError(conn, "invalid request json: "+err.Error())
		protocol.WriteExit(conn, 1)
		return
	}
	if req.Version != protocol.Version {
		protocol.WriteError(conn, fmt.Sprintf("unsupported protocol version %d (agent speaks %d)", req.Version, protocol.Version))
		protocol.WriteExit(conn, 1)
		return
	}

	switch req.Op {
	case protocol.OpPing:
		protocol.WriteExit(conn, 0)
	case protocol.OpInfo:
		a.handleInfo(conn)
	case protocol.OpExec:
		if req.Exec == nil {
			protocol.WriteError(conn, "exec op missing exec payload")
			protocol.WriteExit(conn, 1)
			return
		}
		a.runExec(ctx, conn, req.Exec)
	default:
		protocol.WriteError(conn, fmt.Sprintf("unknown op %q", req.Op))
		protocol.WriteExit(conn, 1)
	}
}

// handleInfo replies with agent + host details as a single stdout frame, then exit 0.
func (a *Agent) handleInfo(conn net.Conn) {
	info := protocol.InfoMessage{
		Version: Version,
		Uname:   unameString(),
		Uptime:  time.Since(startTime).Round(time.Second).String(),
	}
	b, _ := json.Marshal(info)
	if err := protocol.WriteFrame(conn, protocol.FrameStdout, b); err != nil {
		a.log.Warn("control: write info", "error", err)
		return
	}
	protocol.WriteExit(conn, 0)
}

// unameString returns a uname -a style string, falling back to runtime data.
func unameString() string {
	if out, err := exec.Command("uname", "-a").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}
