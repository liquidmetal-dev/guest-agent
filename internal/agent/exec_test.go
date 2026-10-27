package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/protocol"
)

// result collects what a client observed from one control exchange.
type result struct {
	stdout     string
	stderr     string
	errMsg     string
	code       int
	heartbeats int
	fileInfo   *protocol.FileInfo
}

// run drives a single request through handleControl over net.Pipe. For an
// exec with HasStdin, or a put, stdinFrames (each a payload) are sent as
// FrameStdin, followed by FrameStdinEOF.
func run(t *testing.T, req *protocol.Request, stdinFrames ...string) result {
	t.Helper()
	return runFrames(t, req, true, stdinFrames...)
}

// runNoEOF is run without the trailing FrameStdinEOF, for tests that need the
// agent to keep waiting for input.
func runNoEOF(t *testing.T, req *protocol.Request, stdinFrames ...string) result {
	t.Helper()
	return runFrames(t, req, false, stdinFrames...)
}

func runFrames(t *testing.T, req *protocol.Request, sendEOF bool, stdinFrames ...string) result {
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
	if (req.Exec != nil && req.Exec.HasStdin) || req.Put != nil {
		go func() {
			for _, s := range stdinFrames {
				protocol.WriteFrame(client, protocol.FrameStdin, []byte(s))
			}
			if sendEOF {
				protocol.WriteFrame(client, protocol.FrameStdinEOF, nil)
			}
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
		case protocol.FrameHeartbeat:
			res.heartbeats++
		case protocol.FrameFileInfo:
			var fi protocol.FileInfo
			jsonMust(t, f.Payload, &fi)
			res.fileInfo = &fi
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

// TestExecStdoutSingleProc pins the fix for #17/#22. With one scheduler thread,
// as on a 1-vCPU guest, the old cmd.StdoutPipe()-based code let cmd.Wait() close
// the read end before pump had read a fast child's output, returning exit 0 with
// empty stdout. Owning the pipes via os.Pipe() rules that out; on the old code
// this loop fails within a few dozen iterations.
func TestExecStdoutSingleProc(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	for i := 0; i < 300; i++ {
		res := run(t, execReq(&protocol.Exec{Cmd: "echo", Args: []string{"hello"}}))
		if res.stdout != "hello\n" || res.code != 0 {
			t.Fatalf("iteration %d: stdout = %q, code = %d; want %q, 0", i, res.stdout, res.code, "hello\n")
		}
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

func TestExecHeartbeats(t *testing.T) {
	orig := heartbeatInterval
	heartbeatInterval = 50 * time.Millisecond
	defer func() { heartbeatInterval = orig }()

	res := run(t, execReq(&protocol.Exec{Cmd: "sleep", Args: []string{"1"}}))
	if res.heartbeats < 2 {
		t.Errorf("heartbeats = %d, want at least 2 for a quiet 1s command", res.heartbeats)
	}
	if res.code != 0 {
		t.Errorf("code = %d, want 0", res.code)
	}
}

// TestExecHeartbeatsContinueAfterOutputRedirected covers a command that
// closes its own stdout/stderr immediately (e.g. via `exec >/dev/null`)
// while continuing to run: heartbeats must keep flowing until the process
// itself exits, not stop as soon as the agent-facing pipes drain.
func TestExecHeartbeatsContinueAfterOutputRedirected(t *testing.T) {
	orig := heartbeatInterval
	heartbeatInterval = 50 * time.Millisecond
	defer func() { heartbeatInterval = orig }()

	res := run(t, execReq(&protocol.Exec{
		Cmd:   `exec >/dev/null 2>&1; sleep 1`,
		Shell: true,
	}))
	if res.heartbeats < 2 {
		t.Errorf("heartbeats = %d, want at least 2 even with output redirected away", res.heartbeats)
	}
	if res.code != 0 {
		t.Errorf("code = %d, want 0", res.code)
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
