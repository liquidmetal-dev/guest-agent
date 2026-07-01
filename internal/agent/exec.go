package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/protocol"
)

// killGrace is how long a process group gets after SIGTERM before SIGKILL.
const killGrace = 5 * time.Second

// runExec runs e and streams its stdin/stdout/stderr over conn, ending with an
// exit frame. The child runs in its own process group so the whole tree is
// reaped on timeout or host disconnect.
func (a *Agent) runExec(ctx context.Context, conn net.Conn, e *protocol.Exec) {
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if e.TimeoutSec > 0 {
		var stop context.CancelFunc
		execCtx, stop = context.WithTimeout(execCtx, time.Duration(e.TimeoutSec)*time.Second)
		defer stop()
	}

	cmd, err := buildCmd(e)
	if err != nil {
		protocol.WriteError(conn, err.Error())
		protocol.WriteExit(conn, 126)
		return
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		protocol.WriteError(conn, "stdout pipe: "+err.Error())
		protocol.WriteExit(conn, 126)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		protocol.WriteError(conn, "stderr pipe: "+err.Error())
		protocol.WriteExit(conn, 126)
		return
	}
	var stdin io.WriteCloser
	if e.HasStdin {
		if stdin, err = cmd.StdinPipe(); err != nil {
			protocol.WriteError(conn, "stdin pipe: "+err.Error())
			protocol.WriteExit(conn, 126)
			return
		}
	}

	if err := cmd.Start(); err != nil {
		protocol.WriteError(conn, "start: "+err.Error())
		protocol.WriteExit(conn, 127)
		return
	}
	pgid := cmd.Process.Pid // Setpgid makes the group id equal the child pid.

	// Serialize frame writes: stdout/stderr pumps and the final exit frame all
	// share conn.
	var wmu sync.Mutex
	write := func(t protocol.FrameType, p []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		if err := protocol.WriteFrame(conn, t, p); err != nil {
			cancel() // host gone: tear the command down
		}
	}

	var pumps sync.WaitGroup
	pumps.Add(2)
	go pump(&pumps, stdout, protocol.FrameStdout, write)
	go pump(&pumps, stderr, protocol.FrameStderr, write)

	// Read host->agent frames: stdin chunks and disconnect detection.
	go a.readLoop(conn, stdin, cancel)

	// Reaper: on cancel/timeout, TERM the group, then KILL after the grace.
	procDone := make(chan struct{})
	go reap(execCtx, pgid, procDone)

	pumps.Wait() // drain stdout/stderr (pipes close when the child exits)
	waitErr := cmd.Wait()
	close(procDone)

	wmu.Lock()
	protocol.WriteExit(conn, exitCode(waitErr))
	wmu.Unlock()
}

// buildCmd constructs the *exec.Cmd for e: argv-direct by default, or `sh -c`
// when Shell is set. It applies cwd, env, the process-group attribute, and an
// optional user credential.
func buildCmd(e *protocol.Exec) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if e.Shell {
		script := e.Cmd
		if len(e.Args) > 0 {
			script = strings.Join(append([]string{e.Cmd}, e.Args...), " ")
		}
		cmd = exec.Command("sh", "-c", script)
	} else {
		if e.Cmd == "" {
			return nil, errors.New("exec: empty cmd")
		}
		cmd = exec.Command(e.Cmd, e.Args...)
	}
	cmd.Dir = e.Cwd

	env := os.Environ()
	for k, v := range e.Env {
		env = append(env, k+"="+v)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e.User != "" {
		cred, home, err := credentialFor(e.User)
		if err != nil {
			return nil, err
		}
		cmd.SysProcAttr.Credential = cred
		env = append(env, "HOME="+home, "USER="+e.User, "LOGNAME="+e.User)
	}
	cmd.Env = env
	return cmd, nil
}

// credentialFor resolves a system user into a syscall.Credential plus its home.
func credentialFor(name string) (*syscall.Credential, string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", fmt.Errorf("lookup user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, "", fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, "", fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}
	var groups []uint32
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}, u.HomeDir, nil
}

// pump copies a child output pipe into framed writes of type t until EOF.
func pump(wg *sync.WaitGroup, r io.Reader, t protocol.FrameType, write func(protocol.FrameType, []byte)) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			write(t, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// readLoop consumes host->agent frames: stdin chunks go to the child, stdin_eof
// closes its stdin. Any read error (host disconnect) cancels the command.
func (a *Agent) readLoop(conn net.Conn, stdin io.WriteCloser, cancel context.CancelFunc) {
	for {
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			cancel() // host disconnected or closed its write half
			return
		}
		switch f.Type {
		case protocol.FrameStdin:
			if stdin != nil {
				if _, werr := stdin.Write(f.Payload); werr != nil {
					stdin.Close()
					stdin = nil
				}
			}
		case protocol.FrameStdinEOF:
			if stdin != nil {
				stdin.Close()
				stdin = nil
			}
		}
	}
}

// reap waits for ctx cancellation, then SIGTERMs the process group and, after
// killGrace, SIGKILLs it. It returns early if the process finished first.
func reap(ctx context.Context, pgid int, procDone <-chan struct{}) {
	select {
	case <-procDone:
		return
	case <-ctx.Done():
	}
	syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-procDone:
	case <-time.After(killGrace):
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// exitCode maps a cmd.Wait error to a shell-style exit code. A signal-killed
// process yields 128+signo.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}
