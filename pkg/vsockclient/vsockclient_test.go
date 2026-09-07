package vsockclient_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func TestDial_CloseWrite(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "vm.vsock")

	l, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer l.Close()

	const port = 1024

	serverDone := make(chan error, 1)

	go func() {
		conn, err := l.Accept()
		if err != nil {
			serverDone <- err

			return
		}
		defer conn.Close()

		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			serverDone <- err

			return
		}
		if strings.TrimSpace(line) != fmt.Sprintf("CONNECT %d", port) {
			serverDone <- fmt.Errorf("unexpected connect line %q", line)

			return
		}
		fmt.Fprintf(conn, "OK 0\n")

		buf := make([]byte, 4)
		if _, err := io.ReadFull(r, buf); err != nil {
			serverDone <- fmt.Errorf("read ping: %w", err)

			return
		}
		if string(buf) != "ping" {
			serverDone <- fmt.Errorf("expected %q, got %q", "ping", buf)

			return
		}

		// The client half-closed after "ping" - confirm we observe EOF here.
		if _, err := r.Read(buf); err != io.EOF {
			serverDone <- fmt.Errorf("expected io.EOF after client half-close, got %v", err)

			return
		}

		// The read side being closed shouldn't stop us writing a reply.
		if _, err := conn.Write([]byte("pong")); err != nil {
			serverDone <- fmt.Errorf("write pong: %w", err)

			return
		}

		serverDone <- nil
	}()

	conn, err := vsockclient.Dial(context.Background(), udsPath, port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("%T does not implement CloseWrite", conn)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	buf := make([]byte, 4)
	if _, err := readFull(conn, buf); err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if string(buf) != "pong" {
		t.Fatalf("expected %q, got %q", "pong", buf)
	}

	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
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
