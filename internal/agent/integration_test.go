package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/protocol"
	"github.com/liquidmetal-dev/guest-agent/internal/transport"
)

func jsonMust(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
}

// freePort returns a currently-free TCP port on 127.0.0.1.
func freePort(t *testing.T) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

func dialUntilReady(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startAgent runs an Agent on tcp with the given ssh target, returning the
// control and ssh addresses and a stop func.
func startAgent(t *testing.T, sshTarget string) (control, ssh string, stop func()) {
	t.Helper()
	cp, sp := freePort(t), freePort(t)
	cfg := Config{
		Transport:   transport.Config{Kind: transport.TCP, Addr: "127.0.0.1"},
		ControlPort: cp,
		SSHPort:     sp,
		SSHTarget:   sshTarget,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { New(cfg).Run(ctx); close(done) }()
	control = net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(cp), 10))
	ssh = net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(sp), 10))
	return control, ssh, func() { cancel(); <-done }
}

// TestExecOverTCP exercises the full transport+control path against a real
// command, matching how vsock-connect drives the agent.
func TestExecOverTCP(t *testing.T) {
	control, _, stop := startAgent(t, "127.0.0.1:1")
	defer stop()

	conn := dialUntilReady(t, control)
	defer conn.Close()

	req := execReq(&protocol.Exec{Cmd: "echo", Args: []string{"over-tcp"}})
	if err := protocol.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	var out string
	var code int
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if f.Type == protocol.FrameStdout {
			out += string(f.Payload)
		}
		if f.Type == protocol.FrameExit {
			var ex protocol.ExitMessage
			jsonMust(t, f.Payload, &ex)
			code = ex.Code
			break
		}
	}
	if out != "over-tcp\n" || code != 0 {
		t.Errorf("out=%q code=%d", out, code)
	}
}

// TestSSHProxy verifies the ssh port forwards bytes to the configured target.
// A fake "sshd" echoes whatever it receives.
func TestSSHProxy(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	_, ssh, stop := startAgent(t, echo.Addr().String())
	defer stop()

	conn := dialUntilReady(t, ssh)
	defer conn.Close()

	msg := []byte("ping-through-proxy")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(msg) {
		t.Errorf("got %q, want %q", buf, msg)
	}
}
