package vsockclient_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/agent"
	"github.com/liquidmetal-dev/guest-agent/internal/transport"
	"github.com/liquidmetal-dev/guest-agent/pkg/vsockclient"
)

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

func TestDialTCP_Ping(t *testing.T) {
	controlPort := freePort(t)
	sshPort := freePort(t)

	a := agent.New(agent.Config{
		Transport:   transport.Config{Kind: transport.TCP, Addr: "127.0.0.1"},
		ControlPort: controlPort,
		SSHPort:     sshPort,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent.Run: %v", err)
		}
	})

	addr := fmt.Sprintf("127.0.0.1:%d", controlPort)

	conn := dialUntilReady(t, addr)
	defer conn.Close()

	req := &vsockclient.Request{Version: vsockclient.Version, Op: vsockclient.OpPing}
	if err := vsockclient.WriteRequest(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	f, err := vsockclient.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.Type != vsockclient.FrameExit {
		t.Fatalf("expected exit frame, got %s", f.Type)
	}
	var ex vsockclient.ExitMessage
	if err := json.Unmarshal(f.Payload, &ex); err != nil {
		t.Fatalf("unmarshal exit: %v", err)
	}
	if ex.Code != 0 {
		t.Fatalf("expected exit code 0, got %d", ex.Code)
	}
}

func TestDial_UDSHandshake(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "vm.vsock")

	l, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer l.Close()

	const port = 1024
	const greeting = "hello-through-the-handshake"

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(line) != fmt.Sprintf("CONNECT %d", port) {
			fmt.Fprintf(conn, "ERR unexpected %q\n", line)
			return
		}
		fmt.Fprintf(conn, "OK 0\n")
		conn.Write([]byte(greeting))
	}()

	conn, err := vsockclient.Dial(context.Background(), udsPath, port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, len(greeting))
	if _, err := readFull(conn, buf); err != nil {
		t.Fatalf("read post-handshake bytes: %v", err)
	}
	if string(buf) != greeting {
		t.Fatalf("expected %q, got %q", greeting, buf)
	}
}

func TestDial_UDSHandshakeRejectsLooseMatch(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "vm.vsock")

	l, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer l.Close()

	const port = 1024

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		r := bufio.NewReader(conn)
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		// "OKAY" merely starts with "OK" but is not the exact token the
		// handshake must require.
		fmt.Fprintf(conn, "OKAY 0\n")
	}()

	if _, err := vsockclient.Dial(context.Background(), udsPath, port); err == nil {
		t.Fatal("expected Dial to reject a non-exact \"OK\" handshake reply, got nil error")
	}
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func dialUntilReady(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := vsockclient.DialTCP(context.Background(), addr)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", addr, err)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			time.Sleep(10 * time.Millisecond)
		}
	}
}
