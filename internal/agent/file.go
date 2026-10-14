package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/protocol"
)

// Put and get run on the connection's own goroutine with no heartbeat frames:
// the data stream itself is the liveness signal (the host sends during a put,
// the agent during a get), so a heartbeat would add nothing.

// fileChunk is the payload size of one streamed data frame.
const fileChunk = 32 * 1024

// errHostGone marks a conn read/write failure: the host disconnected, so no
// reply is attempted.
var errHostGone = errors.New("host disconnected")

// fileOp carries the per-operation context for a put or get. Its context ends
// on timeout or agent shutdown, and an AfterFunc then sets a conn deadline so
// whatever ReadFrame/WriteFrame is in flight returns instead of blocking.
type fileOp struct {
	ctx       context.Context
	conn      net.Conn
	cancel    context.CancelFunc
	stopAfter func() bool
}

func newFileOp(ctx context.Context, conn net.Conn, timeoutSec int) *fileOp {
	opCtx, cancel := context.WithCancel(ctx)
	if timeoutSec > 0 {
		var stop context.CancelFunc
		opCtx, stop = context.WithTimeout(opCtx, time.Duration(timeoutSec)*time.Second)
		prev := cancel
		cancel = func() { stop(); prev() }
	}
	o := &fileOp{ctx: opCtx, conn: conn, cancel: cancel}
	o.stopAfter = context.AfterFunc(opCtx, func() { conn.SetDeadline(time.Now()) })
	return o
}

// finish sends the outcome of the op to the host and releases the op.
func (o *fileOp) finish(a *Agent, op string, info *protocol.FileInfo, err error) {
	o.stopAfter()
	defer o.cancel()
	o.conn.SetDeadline(time.Time{})

	switch {
	case err == nil:
		if werr := protocol.WriteFileInfo(o.conn, *info); werr != nil {
			return
		}
		protocol.WriteExit(o.conn, 0)
	case o.ctx.Err() != nil:
		msg := "timeout"
		if !errors.Is(o.ctx.Err(), context.DeadlineExceeded) {
			msg = "agent shutting down"
		}
		// Best effort: the host may itself be stuck, so bound the reply.
		o.conn.SetDeadline(time.Now().Add(killGrace))
		protocol.WriteError(o.conn, op+": "+msg)
		protocol.WriteExit(o.conn, 1)
	case errors.Is(err, errHostGone):
		a.log.Debug(op+": host disconnected", "error", err)
	default:
		protocol.WriteError(o.conn, op+": "+err.Error())
		protocol.WriteExit(o.conn, 1)
	}
}

// runPut handles OpPut: stream stdin frames into a temp file beside p.Path,
// then rename it into place.
func (a *Agent) runPut(ctx context.Context, conn net.Conn, p *protocol.Put) {
	o := newFileOp(ctx, conn, p.TimeoutSec)
	info, err := o.put(p)
	o.finish(a, "put", info, err)
}

// runGet handles OpGet: stream the file as stdout frames.
func (a *Agent) runGet(ctx context.Context, conn net.Conn, g *protocol.Get) {
	o := newFileOp(ctx, conn, g.TimeoutSec)
	info, err := o.get(g)
	o.finish(a, "get", info, err)
}

func (o *fileOp) put(p *protocol.Put) (*protocol.FileInfo, error) {
	if !filepath.IsAbs(p.Path) {
		return nil, fmt.Errorf("path %q is not absolute", p.Path)
	}

	// Defaults: explicit fields win, then an existing file's mode and owner,
	// then 0644 owned by the agent itself.
	mode := p.Mode & 0o7777
	uid, gid := -1, -1
	if fi, err := os.Stat(p.Path); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if mode == 0 {
				mode = st.Mode & 0o7777
			}
			if p.User == "" {
				uid = int(st.Uid)
				if p.Group == "" {
					gid = int(st.Gid)
				}
			}
		}
	}
	if mode == 0 {
		mode = 0o644
	}
	ruid, rgid, err := resolveOwner(p.User, p.Group)
	if err != nil {
		return nil, err
	}
	if ruid >= 0 {
		uid = ruid
	}
	if rgid >= 0 {
		gid = rgid
	}

	dir := filepath.Dir(p.Path)
	if p.MkdirParents {
		if err := mkdirParents(dir, uid, gid); err != nil {
			return nil, err
		}
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p.Path)+".*")
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmp.Name())
		}
	}()

	for done := false; !done; {
		f, err := protocol.ReadFrame(o.conn)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errHostGone, err)
		}
		switch f.Type {
		case protocol.FrameStdin:
			if _, err := tmp.Write(f.Payload); err != nil {
				return nil, err
			}
		case protocol.FrameStdinEOF:
			done = true
		}
	}

	// chown first: it clears setuid/setgid bits, which chmod then restores.
	if uid >= 0 || gid >= 0 {
		if err := tmp.Chown(uid, gid); err != nil {
			return nil, err
		}
	}
	if err := syscall.Fchmod(int(tmp.Fd()), mode); err != nil {
		return nil, &os.PathError{Op: "chmod", Path: tmp.Name(), Err: err}
	}
	if err := tmp.Sync(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), p.Path); err != nil {
		return nil, err
	}
	committed = true

	fi, err := os.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	info := fileInfoOf(fi)
	return &info, nil
}

func (o *fileOp) get(g *protocol.Get) (*protocol.FileInfo, error) {
	if !filepath.IsAbs(g.Path) {
		return nil, fmt.Errorf("path %q is not absolute", g.Path)
	}
	f, err := os.Open(g.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s: is a directory", g.Path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", g.Path)
	}

	buf := make([]byte, fileChunk)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if werr := protocol.WriteFrame(o.conn, protocol.FrameStdout, buf[:n]); werr != nil {
				return nil, fmt.Errorf("%w: %v", errHostGone, werr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	info := fileInfoOf(fi)
	return &info, nil
}

// resolveOwner turns name-or-numeric user and group strings into ids; -1
// means "not specified". A user without a group yields the user's primary
// group when the user is known to the system.
func resolveOwner(userName, groupName string) (uid, gid int, err error) {
	uid, gid = -1, -1
	if userName != "" {
		var u *user.User
		if n, perr := strconv.ParseUint(userName, 10, 32); perr == nil {
			uid = int(n)
			u, _ = user.LookupId(userName) // best effort, for the primary group
		} else if u, err = user.Lookup(userName); err != nil {
			return -1, -1, fmt.Errorf("lookup user %q: %w", userName, err)
		}
		if u != nil {
			if uid, err = parseID(u.Uid); err != nil {
				return -1, -1, fmt.Errorf("parse uid %q: %w", u.Uid, err)
			}
			if gid, err = parseID(u.Gid); err != nil {
				return -1, -1, fmt.Errorf("parse gid %q: %w", u.Gid, err)
			}
		}
	}
	if groupName != "" {
		if n, perr := strconv.ParseUint(groupName, 10, 32); perr == nil {
			gid = int(n)
		} else {
			g, err := user.LookupGroup(groupName)
			if err != nil {
				return -1, -1, fmt.Errorf("lookup group %q: %w", groupName, err)
			}
			if gid, err = parseID(g.Gid); err != nil {
				return -1, -1, fmt.Errorf("parse gid %q: %w", g.Gid, err)
			}
		}
	}
	return uid, gid, nil
}

func parseID(s string) (int, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	return int(n), err
}

// mkdirParents creates dir and any missing ancestors with mode 0755, giving
// each directory it created the file's owner so a sandbox user can create
// siblings later.
func mkdirParents(dir string, uid, gid int) error {
	var created []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		created = append(created, d)
		if d == filepath.Dir(d) {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i := len(created) - 1; i >= 0; i-- {
		if err := os.Chmod(created[i], 0o755); err != nil {
			return err
		}
		if uid >= 0 || gid >= 0 {
			if err := os.Chown(created[i], uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}

// fileInfoOf builds the wire FileInfo for fi using the raw Unix mode bits.
func fileInfoOf(fi os.FileInfo) protocol.FileInfo {
	info := protocol.FileInfo{
		Size:  fi.Size(),
		Mode:  uint32(fi.Mode().Perm()),
		Mtime: fi.ModTime().UTC().Format(time.RFC3339),
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		info.Mode = st.Mode & 0o7777
		info.UID = st.Uid
		info.GID = st.Gid
	}
	return info
}
