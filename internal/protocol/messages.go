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
	// OpPut writes a file into the guest from streamed stdin frames.
	OpPut Op = "put"
	// OpGet reads a file out of the guest as streamed stdout frames.
	OpGet Op = "get"
)

// Request is the JSON envelope carried by a FrameRequest. Exactly one of the
// op-specific fields is populated, matching Op.
type Request struct {
	Version int   `json:"version"`
	Op      Op    `json:"op"`
	Exec    *Exec `json:"exec,omitempty"`
	Put     *Put  `json:"put,omitempty"`
	Get     *Get  `json:"get,omitempty"`
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

// Put describes a file to write. After the request the host streams the
// content as FrameStdin frames and ends it with FrameStdinEOF, keeping its
// write side open until it has read the exit frame. The agent writes a
// temporary file beside Path and renames it into place, so a reader never
// sees a partial file. On success it sends FrameFileInfo then FrameExit{0}.
type Put struct {
	// Path is the absolute destination path.
	Path string `json:"path"`
	// Mode holds the permission bits (0o7777 range). 0 keeps an existing
	// file's mode, or 0644 for a new file.
	Mode uint32 `json:"mode,omitempty"`
	// User is the owner as a name or numeric uid. Empty keeps an existing
	// file's uid, or the agent's own uid for a new file.
	User string `json:"user,omitempty"`
	// Group is the group as a name or numeric gid. Empty means the primary
	// group of User when User is set, else an existing file's gid, else the
	// agent's own gid.
	Group string `json:"group,omitempty"`
	// MkdirParents creates missing parent directories (0755, same owner).
	MkdirParents bool `json:"mkdir_parents,omitempty"`
	// TimeoutSec bounds the whole transfer; 0 means unbounded.
	TimeoutSec int `json:"timeout_sec,omitempty"`
}

// Get describes a regular file to read. The agent streams its content as
// FrameStdout frames, then sends FrameFileInfo and FrameExit{0}. Directories
// and special files are an error.
type Get struct {
	// Path is the absolute source path; symlinks are followed.
	Path string `json:"path"`
	// TimeoutSec bounds the whole transfer; 0 means unbounded.
	TimeoutSec int `json:"timeout_sec,omitempty"`
}

// FileInfo is the JSON payload of a FrameFileInfo.
type FileInfo struct {
	Size int64 `json:"size"`
	// Mode holds the Unix permission bits (st_mode & 0o7777).
	Mode uint32 `json:"mode"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
	// Mtime is the modification time in RFC 3339 format.
	Mtime string `json:"mtime"`
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
	// Ops lists the operations this agent supports, so a host can detect an
	// agent that predates put/get (such an agent omits the field).
	Ops []string `json:"ops"`
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

// WriteFileInfo encodes fi as a FrameFileInfo on w.
func WriteFileInfo(w io.Writer, fi FileInfo) error {
	b, err := json.Marshal(fi)
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameFileInfo, b)
}

// WriteError encodes msg as a FrameError on w.
func WriteError(w io.Writer, msg string) error {
	b, err := json.Marshal(ErrorMessage{Msg: msg})
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameError, b)
}
