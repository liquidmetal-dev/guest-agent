package protocol

import (
	"encoding/json"
	"io"
)

// Version is the current protocol major version. The agent rejects requests
// whose Version differs.
const Version = 1

// Op identifies a control-channel operation.
type Op string

const (
	// OpExec runs a command on the guest.
	OpExec Op = "exec"
	// OpPing is a liveness check; the agent replies with exit code 0.
	OpPing Op = "ping"
	// OpInfo returns agent version and host details.
	OpInfo Op = "info"
)

// Request is the JSON envelope carried by a FrameRequest. Exactly one of the
// op-specific fields is populated, matching Op.
type Request struct {
	Version int   `json:"version"`
	Op      Op    `json:"op"`
	Exec    *Exec `json:"exec,omitempty"`
}

// Exec describes a command to run.
type Exec struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args,omitempty"`
	// Cwd is the working directory; empty means the agent's cwd.
	Cwd string `json:"cwd,omitempty"`
	// Env entries are added to (and override) the agent's environment.
	Env map[string]string `json:"env,omitempty"`
	// Shell runs `sh -c "<cmd + args joined>"` instead of a direct exec.
	Shell bool `json:"shell,omitempty"`
	// User, if set, runs the command as that system user (uid/gid/groups/HOME).
	User string `json:"user,omitempty"`
	// TimeoutSec bounds the run; 0 means unbounded.
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// HasStdin tells the agent to expect streamed stdin frames.
	HasStdin bool `json:"has_stdin,omitempty"`
}

// ExitMessage is the JSON payload of a FrameExit.
type ExitMessage struct {
	Code int `json:"code"`
}

// ErrorMessage is the JSON payload of a FrameError.
type ErrorMessage struct {
	Msg string `json:"msg"`
}

// InfoMessage is the JSON payload returned by OpInfo. The agent marshals it into
// a single FrameStdout, then sends FrameExit{0}.
type InfoMessage struct {
	Version string `json:"version"`
	Uname   string `json:"uname"`
	Uptime  string `json:"uptime"`
}

// WriteRequest encodes req into a FrameRequest on w.
func WriteRequest(w io.Writer, req *Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameRequest, b)
}

// WriteExit encodes an exit code as a FrameExit on w.
func WriteExit(w io.Writer, code int) error {
	b, err := json.Marshal(ExitMessage{Code: code})
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameExit, b)
}

// WriteError encodes msg as a FrameError on w.
func WriteError(w io.Writer, msg string) error {
	b, err := json.Marshal(ErrorMessage{Msg: msg})
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameError, b)
}
