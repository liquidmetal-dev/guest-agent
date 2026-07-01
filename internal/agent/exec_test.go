package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/protocol"
)

// result collects what a client observed from one control exchange.
type result struct {
	stdout string
	stderr string
	errMsg string
	code   int
}

// run drives a single request through handleControl over net.Pipe. stdinFrames
// (each a payload) are sent as FrameStdin, followed by FrameStdinEOF.
func run(t *testing.T, req *protocol.Request, stdinFrames ...string) result {
	t.Helper()
	client, server := net.Pipe()
	a := New(Config{})

	done := make(chan struct{})
	go func() {
		a.handleControl(context.Background(), server)
		server.Close()
		close(done)
	}()

	if err := protocol.WriteRequest(client, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if req.Exec != nil && req.Exec.HasStdin {
		go func() {
			for _, s := range stdinFrames {
				protocol.WriteFrame(client, protocol.FrameStdin, []byte(s))
			}
			protocol.WriteFrame(client, protocol.FrameStdinEOF, nil)
		}()
	}

	var res result
	for {
		f, err := protocol.ReadFrame(client)
		if err == io.EOF || err == io.ErrClosedPipe {
			break
		}
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch f.Type {
		case protocol.FrameStdout:
			res.stdout += string(f.Payload)
		case protocol.FrameStderr:
			res.stderr += string(f.Payload)
		case protocol.FrameError:
			res.errMsg += string(f.Payload)
		case protocol.FrameExit:
			var ex protocol.ExitMessage
			jsonMust(t, f.Payload, &ex)
			res.code = ex.Code
			client.Close()
			<-done
			return res
		}
	}
	<-done
	return res
}

func execReq(e *protocol.Exec) *protocol.Request {
	return &protocol.Request{Version: protocol.Version, Op: protocol.OpExec, Exec: e}
}

func TestExecStdout(t *testing.T) {
	res := run(t, execReq(&protocol.Exec{Cmd: "echo", Args: []string{"hello"}}))
	if res.stdout != "hello\n" {
		t.Errorf("stdout = %q, want %q", res.stdout, "hello\n")
	}
	if res.code != 0 {
		t.Errorf("code = %d, want 0", res.code)
	}
}

func TestExecExitCode(t *testing.T) {
	res := run(t, execReq(&protocol.Exec{Cmd: "false"}))
	if res.code != 1 {
		t.Errorf("code = %d, want 1", res.code)
	}
}

func TestExecStderrSeparation(t *testing.T) {
	res := run(t, execReq(&protocol.Exec{Cmd: "echo oops 1>&2", Shell: true}))
	if res.stderr != "oops\n" {
		t.Errorf("stderr = %q, want %q", res.stderr, "oops\n")
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
}

func TestExecStdinStreaming(t *testing.T) {
	res := run(t, execReq(&protocol.Exec{Cmd: "cat", HasStdin: true}), "foo", "bar")
	if res.stdout != "foobar" {
		t.Errorf("stdout = %q, want %q", res.stdout, "foobar")
	}
	if res.code != 0 {
		t.Errorf("code = %d, want 0", res.code)
	}
}

type failingWriteCloser struct{}

func (failingWriteCloser) Write([]byte) (int, error) {
	return 0, errors.New("stdin closed")
}

func (failingWriteCloser) Close() error { return nil }

func TestReadLoopKeepsDisconnectDetectionAfterStdinWriteError(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	cancelled := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() { close(cancelled) })
	}

	go New(Config{}).readLoop(server, failingWriteCloser{}, cancel)

	if err := protocol.WriteFrame(client, protocol.FrameStdin, []byte("ignored")); err != nil {
		t.Fatalf("write stdin frame: %v", err)
	}
	select {
	case <-cancelled:
		t.Fatal("readLoop cancelled on stdin write error; want it to keep reading")
	case <-time.After(100 * time.Millisecond):
	}

	client.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not cancel after host disconnect")
	}
}

func TestExecEnvAndCwd(t *testing.T) {
	res := run(t, execReq(&protocol.Exec{
		Cmd: "sh", Args: []string{"-c", "echo $FOO in $(pwd)"},
		Env: map[string]string{"FOO": "bar"}, Cwd: "/tmp",
	}))
	if res.stdout != "bar in /tmp\n" {
		t.Errorf("stdout = %q, want %q", res.stdout, "bar in /tmp\n")
	}
}

func TestExecTimeoutKills(t *testing.T) {
	start := time.Now()
	res := run(t, execReq(&protocol.Exec{Cmd: "sleep", Args: []string{"30"}, TimeoutSec: 1}))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v, expected ~1s timeout", elapsed)
	}
	// SIGTERM (15) -> 128+15 = 143.
	if res.code != 143 {
		t.Errorf("code = %d, want 143 (SIGTERM)", res.code)
	}
}

func TestPing(t *testing.T) {
	res := run(t, &protocol.Request{Version: protocol.Version, Op: protocol.OpPing})
	if res.code != 0 {
		t.Errorf("code = %d, want 0", res.code)
	}
}

func TestInfo(t *testing.T) {
	res := run(t, &protocol.Request{Version: protocol.Version, Op: protocol.OpInfo})
	if res.code != 0 || res.stdout == "" {
		t.Errorf("info: code=%d stdout=%q", res.code, res.stdout)
	}
}

func TestVersionMismatch(t *testing.T) {
	res := run(t, &protocol.Request{Version: 999, Op: protocol.OpPing})
	if res.code == 0 || res.errMsg == "" {
		t.Errorf("expected error+nonzero exit, got code=%d err=%q", res.code, res.errMsg)
	}
}

func TestUnknownOp(t *testing.T) {
	res := run(t, &protocol.Request{Version: protocol.Version, Op: "bogus"})
	if res.code == 0 || res.errMsg == "" {
		t.Errorf("expected error+nonzero exit, got code=%d err=%q", res.code, res.errMsg)
	}
}
