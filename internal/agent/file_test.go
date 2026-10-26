package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/protocol"
)

func putReq(p *protocol.Put) *protocol.Request {
	return &protocol.Request{Version: protocol.Version, Op: protocol.OpPut, Put: p}
}

func getReq(g *protocol.Get) *protocol.Request {
	return &protocol.Request{Version: protocol.Version, Op: protocol.OpGet, Get: g}
}

// withUmask runs the test under a restrictive umask so a mode that survives
// proves the agent applied it explicitly rather than relying on the default.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func statOwner(t *testing.T, path string) (uid, gid uint32) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return st.Uid, st.Gid
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestPutCreatesFile(t *testing.T) {
	withUmask(t, 0o077)
	path := filepath.Join(t.TempDir(), "hello.txt")

	res := run(t, putReq(&protocol.Put{Path: path, Mode: 0o640}), "hel", "lo")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
	if m := statMode(t, path); m != 0o640 {
		t.Errorf("mode = %o, want 640", m)
	}
	if res.fileInfo == nil {
		t.Fatal("no file_info frame")
	}
	if res.fileInfo.Size != 5 || res.fileInfo.Mode != 0o640 {
		t.Errorf("file_info = %+v, want size 5 mode 640", res.fileInfo)
	}
}

func TestPutDefaultMode(t *testing.T) {
	withUmask(t, 0o077)
	path := filepath.Join(t.TempDir(), "new.txt")

	res := run(t, putReq(&protocol.Put{Path: path}), "x")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	if m := statMode(t, path); m != 0o644 {
		t.Errorf("mode = %o, want 644", m)
	}
}

func TestPutInheritsExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := run(t, putReq(&protocol.Put{Path: path}), "new")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	if m := statMode(t, path); m != 0o755 {
		t.Errorf("mode = %o, want 755 inherited from the replaced file", m)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

func TestPutOwnerNumericSelf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mine")
	uid, gid := os.Getuid(), os.Getgid()

	res := run(t, putReq(&protocol.Put{Path: path, User: strconv.Itoa(uid), Group: strconv.Itoa(gid)}), "x")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	if u, g := statOwner(t, path); int(u) != uid || int(g) != gid {
		t.Errorf("owner = %d:%d, want %d:%d", u, g, uid, gid)
	}
	if res.fileInfo == nil || int(res.fileInfo.UID) != uid || int(res.fileInfo.GID) != gid {
		t.Errorf("file_info = %+v, want uid %d gid %d", res.fileInfo, uid, gid)
	}
}

func TestPutGroupOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grp")
	uid, gid := os.Getuid(), os.Getgid()

	res := run(t, putReq(&protocol.Put{Path: path, Group: strconv.Itoa(gid)}), "x")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	if u, g := statOwner(t, path); int(u) != uid || int(g) != gid {
		t.Errorf("owner = %d:%d, want %d:%d", u, g, uid, gid)
	}
}

func TestPutOwnerByName(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to another user needs root")
	}
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("no user nobody")
	}
	path := filepath.Join(t.TempDir(), "nobodys")

	res := run(t, putReq(&protocol.Put{Path: path, User: "nobody"}), "x")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	gotUID, gotGID := statOwner(t, path)
	if strconv.Itoa(int(gotUID)) != u.Uid || strconv.Itoa(int(gotGID)) != u.Gid {
		t.Errorf("owner = %d:%d, want %s:%s (nobody and its primary group)", gotUID, gotGID, u.Uid, u.Gid)
	}
}

func TestPutOwnerUnknownUser(t *testing.T) {
	dir := t.TempDir()

	res := run(t, putReq(&protocol.Put{Path: filepath.Join(dir, "f"), User: "no-such-user-xyz"}), "x")

	if res.code != 1 || !strings.Contains(res.errMsg, "lookup user") {
		t.Errorf("code = %d err = %q, want 1 and a lookup user error", res.code, res.errMsg)
	}
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Errorf("dir not empty after failed put: %v", ents)
	}
}

func TestPutMkdirParents(t *testing.T) {
	withUmask(t, 0o077)
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c.txt")
	uid, gid := os.Getuid(), os.Getgid()

	res := run(t, putReq(&protocol.Put{
		Path: path, MkdirParents: true,
		User: strconv.Itoa(uid), Group: strconv.Itoa(gid),
	}), "deep")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "deep" {
		t.Errorf("content = %q", got)
	}
	for _, d := range []string{filepath.Join(dir, "a"), filepath.Join(dir, "a", "b")} {
		if m := statMode(t, d); m != 0o755 {
			t.Errorf("%s mode = %o, want 755", d, m)
		}
		if u, g := statOwner(t, d); int(u) != uid || int(g) != gid {
			t.Errorf("%s owner = %d:%d, want %d:%d", d, u, g, uid, gid)
		}
	}
}

func TestPutMissingDirErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "f.txt")

	res := run(t, putReq(&protocol.Put{Path: path}), "x")

	if res.code != 1 || !strings.Contains(res.errMsg, "no such file or directory") {
		t.Errorf("code = %d err = %q, want 1 and ENOENT text", res.code, res.errMsg)
	}
	if res.fileInfo != nil {
		t.Error("file_info sent on failure")
	}
}

func TestPutRelativePathErrors(t *testing.T) {
	res := run(t, putReq(&protocol.Put{Path: "relative/f.txt"}), "x")

	if res.code != 1 || !strings.Contains(res.errMsg, "absolute") {
		t.Errorf("code = %d err = %q, want 1 and an absolute-path error", res.code, res.errMsg)
	}
}

func TestPutReplacesExistingAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := run(t, putReq(&protocol.Put{Path: path}), "new")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
	if ents := dirEntries(t, dir); len(ents) != 1 || ents[0] != "f.txt" {
		t.Errorf("dir entries = %v, want only f.txt (no temp file left)", ents)
	}
}

func TestPutReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	res := run(t, putReq(&protocol.Put{Path: link}), "via-link")

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("link path mode = %v, want a regular file replacing the link", fi.Mode())
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("mode = %o, want 600 inherited from the link's target", m)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "target" {
		t.Errorf("link target content = %q, want untouched %q", got, "target")
	}
}

func TestPutHostDisconnectLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		New(Config{}).handleControl(context.Background(), server)
		server.Close()
		close(done)
	}()

	if err := protocol.WriteRequest(client, putReq(&protocol.Put{Path: filepath.Join(dir, "f")})); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(client, protocol.FrameStdin, []byte("partial")); err != nil {
		t.Fatal(err)
	}
	client.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after host disconnect")
	}
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Errorf("dir entries after disconnect = %v, want none", ents)
	}
}

func TestPutTimeout(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()

	res := runNoEOF(t, putReq(&protocol.Put{Path: filepath.Join(dir, "f"), TimeoutSec: 1}), "partial")

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v, expected ~1s timeout", elapsed)
	}
	if res.code != 1 || !strings.Contains(res.errMsg, "timeout") {
		t.Errorf("code = %d err = %q, want 1 and a timeout error", res.code, res.errMsg)
	}
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Errorf("dir entries after timeout = %v, want none", ents)
	}
}

func TestPutAgentShutdown(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		New(Config{}).handleControl(ctx, server)
		server.Close()
		close(done)
	}()

	if err := protocol.WriteRequest(client, putReq(&protocol.Put{Path: filepath.Join(dir, "f")})); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(client, protocol.FrameStdin, []byte("partial")); err != nil {
		t.Fatal(err)
	}
	cancel()

	code, errMsg := readExit(t, client)
	client.Close()
	<-done

	if code != 1 || !strings.Contains(errMsg, "shutting down") {
		t.Errorf("code = %d err = %q, want 1 and a shutting down error", code, errMsg)
	}
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Errorf("dir entries after shutdown = %v, want none", ents)
	}
}

func TestGetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big")
	content := strings.Repeat("0123456789abcdef", 5000) // 80000 bytes, > one 32 KiB chunk
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}

	res := run(t, getReq(&protocol.Get{Path: path}))

	if res.code != 0 {
		t.Fatalf("code = %d, err = %q", res.code, res.errMsg)
	}
	if res.stdout != content {
		t.Errorf("stdout len = %d, want %d (content mismatch)", len(res.stdout), len(content))
	}
	if res.fileInfo == nil {
		t.Fatal("no file_info frame")
	}
	if res.fileInfo.Size != int64(len(content)) || res.fileInfo.Mode != uint32(statMode(t, path)) {
		t.Errorf("file_info = %+v, want size %d mode %o", res.fileInfo, len(content), statMode(t, path))
	}
}

func TestGetFileInfoMatchesStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)

	res := run(t, getReq(&protocol.Get{Path: path}))

	if res.code != 0 || res.fileInfo == nil {
		t.Fatalf("code = %d err = %q fileInfo = %v", res.code, res.errMsg, res.fileInfo)
	}
	want := protocol.FileInfo{
		Size: 1, Mode: 0o600, UID: st.Uid, GID: st.Gid,
		Mtime: fi.ModTime().UTC().Format(time.RFC3339),
	}
	if *res.fileInfo != want {
		t.Errorf("file_info = %+v, want %+v", *res.fileInfo, want)
	}
}

func TestGetDirectoryErrors(t *testing.T) {
	res := run(t, getReq(&protocol.Get{Path: t.TempDir()}))

	if res.code != 1 || !strings.Contains(res.errMsg, "is a directory") {
		t.Errorf("code = %d err = %q, want 1 and is a directory", res.code, res.errMsg)
	}
	if res.stdout != "" || res.fileInfo != nil {
		t.Errorf("stdout = %q fileInfo = %v, want none", res.stdout, res.fileInfo)
	}
}

func TestGetMissingFileErrors(t *testing.T) {
	res := run(t, getReq(&protocol.Get{Path: filepath.Join(t.TempDir(), "nope")}))

	if res.code != 1 || !strings.Contains(res.errMsg, "no such file or directory") {
		t.Errorf("code = %d err = %q, want 1 and ENOENT text", res.code, res.errMsg)
	}
}

func TestGetRelativePathErrors(t *testing.T) {
	res := run(t, getReq(&protocol.Get{Path: "etc/hostname"}))

	if res.code != 1 || !strings.Contains(res.errMsg, "absolute") {
		t.Errorf("code = %d err = %q, want 1 and an absolute-path error", res.code, res.errMsg)
	}
}

func TestInfoListsOps(t *testing.T) {
	res := run(t, &protocol.Request{Version: protocol.Version, Op: protocol.OpInfo})

	var info protocol.InfoMessage
	if err := json.Unmarshal([]byte(res.stdout), &info); err != nil {
		t.Fatalf("info json %q: %v", res.stdout, err)
	}
	want := []string{"exec", "ping", "info", "put", "get"}
	if strings.Join(info.Ops, ",") != strings.Join(want, ",") {
		t.Errorf("ops = %v, want %v", info.Ops, want)
	}
	if strings.Join(supportedOps, ",") != strings.Join(want, ",") {
		t.Errorf("supportedOps = %v, want %v", supportedOps, want)
	}
}

// readExit drains agent frames on client until exit, returning the exit code
// and the concatenated error text.
func readExit(t *testing.T, client net.Conn) (code int, errMsg string) {
	t.Helper()
	for {
		f, err := protocol.ReadFrame(client)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		switch f.Type {
		case protocol.FrameError:
			errMsg += string(f.Payload)
		case protocol.FrameExit:
			var ex protocol.ExitMessage
			jsonMust(t, f.Payload, &ex)
			return ex.Code, errMsg
		}
	}
}

// cancelConn cancels a context once a given number of bytes has been read
// from, or written to, the wrapped conn. It lets a test end the handler
// context at a precise point in the exchange.
type cancelConn struct {
	net.Conn
	readLeft  int
	writeLeft int
	cancel    func()
}

func (c *cancelConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.readLeft > 0 {
		if c.readLeft -= n; c.readLeft <= 0 {
			c.cancel()
		}
	}
	return n, err
}

func (c *cancelConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if c.writeLeft > 0 {
		if c.writeLeft -= n; c.writeLeft <= 0 {
			c.cancel()
		}
	}
	return n, err
}

func TestPutCancelledAfterEOFIsNotCommitted(t *testing.T) {
	dir := t.TempDir()
	var wire bytes.Buffer
	if err := protocol.WriteRequest(&wire, putReq(&protocol.Put{Path: filepath.Join(dir, "f")})); err != nil {
		t.Fatal(err)
	}
	protocol.WriteFrame(&wire, protocol.FrameStdin, []byte("data"))
	protocol.WriteFrame(&wire, protocol.FrameStdinEOF, nil)

	// The context ends as the agent reads the last byte of stdin_eof, i.e.
	// after the content arrived but before the file can be committed.
	ctx, cancel := context.WithCancel(context.Background())
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		New(Config{}).handleControl(ctx, &cancelConn{Conn: server, readLeft: wire.Len(), cancel: cancel})
		server.Close()
		close(done)
	}()
	go client.Write(wire.Bytes())

	code, errMsg := readExit(t, client)
	client.Close()
	<-done

	if code != 1 || !strings.Contains(errMsg, "shutting down") {
		t.Errorf("code = %d err = %q, want 1 and a shutting down error", code, errMsg)
	}
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Errorf("dir entries = %v, want none (file must not be committed after cancellation)", ents)
	}
}

func TestGetCancelledBeforeFinishReportsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	protocol.WriteFrame(&wire, protocol.FrameStdout, []byte("payload"))

	// The context ends as the agent writes the last content byte: the data
	// is all out, but the op is over its time and must say so.
	ctx, cancel := context.WithCancel(context.Background())
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		New(Config{}).handleControl(ctx, &cancelConn{Conn: server, writeLeft: wire.Len(), cancel: cancel})
		server.Close()
		close(done)
	}()
	if err := protocol.WriteRequest(client, getReq(&protocol.Get{Path: path})); err != nil {
		t.Fatal(err)
	}

	code, errMsg := readExit(t, client)
	client.Close()
	<-done

	if code != 1 || !strings.Contains(errMsg, "shutting down") {
		t.Errorf("code = %d err = %q, want 1 and a shutting down error", code, errMsg)
	}
}
