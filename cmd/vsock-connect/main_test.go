package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liquidmetal-dev/guest-agent/pkg/vsockclient"
)

func TestParsePut(t *testing.T) {
	p, src, err := parsePut([]string{"--mode", "0600", "--owner", "ubuntu:dev", "--mkdir", "--timeout", "5", "src.txt", "/dst"})
	if err != nil {
		t.Fatalf("parsePut: %v", err)
	}
	if src != "src.txt" {
		t.Errorf("src = %q, want src.txt", src)
	}
	if p.Path != "/dst" || p.Mode != 0o600 || p.User != "ubuntu" || p.Group != "dev" || !p.MkdirParents || p.TimeoutSec != 5 {
		t.Errorf("put = %+v", p)
	}
}

func TestParsePutOwnerWithoutGroup(t *testing.T) {
	p, _, err := parsePut([]string{"--owner", "ubuntu", "-", "/dst"})
	if err != nil {
		t.Fatalf("parsePut: %v", err)
	}
	if p.User != "ubuntu" || p.Group != "" {
		t.Errorf("owner = %q:%q, want ubuntu and empty group", p.User, p.Group)
	}
}

func TestParsePutStdinSourceHasNoMode(t *testing.T) {
	p, src, err := parsePut([]string{"-", "/dst"})
	if err != nil {
		t.Fatalf("parsePut: %v", err)
	}
	if src != "-" || p.Mode != 0 {
		t.Errorf("src = %q mode = %o, want - and 0 (agent default)", src, p.Mode)
	}
}

func TestParsePutErrors(t *testing.T) {
	cases := map[string][]string{
		"missing dest":  {"src.txt"},
		"relative dest": {"src.txt", "dst.txt"},
		"bad mode":      {"--mode", "999", "src.txt", "/dst"},
		"unknown flag":  {"--bogus", "src.txt", "/dst"},
		"bad timeout":   {"--timeout", "-1", "src.txt", "/dst"},
		"extra arg":     {"src.txt", "/dst", "more"},
	}
	for name, args := range cases {
		if _, _, err := parsePut(args); err == nil {
			t.Errorf("%s: parsePut(%v) = nil error", name, args)
		}
	}
	if _, _, err := parsePut([]string{"src.txt", "dst.txt"}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative dest error = %v, want mention of absolute", err)
	}
}

func TestParseGet(t *testing.T) {
	g, dest, err := parseGet([]string{"--timeout", "3", "/src"})
	if err != nil {
		t.Fatalf("parseGet: %v", err)
	}
	if g.Path != "/src" || g.TimeoutSec != 3 || dest != "" {
		t.Errorf("get = %+v dest = %q", g, dest)
	}

	g, dest, err = parseGet([]string{"/src", "out.txt"})
	if err != nil {
		t.Fatalf("parseGet: %v", err)
	}
	if g.Path != "/src" || dest != "out.txt" {
		t.Errorf("get = %+v dest = %q", g, dest)
	}

	_, dest, err = parseGet([]string{"/src", "-"})
	if err != nil || dest != "-" {
		t.Errorf("dest = %q err = %v, want -", dest, err)
	}
}

func TestParseGetErrors(t *testing.T) {
	cases := map[string][]string{
		"no src":       {},
		"relative src": {"etc/hostname"},
		"bad timeout":  {"--timeout", "x", "/src"},
		"extra arg":    {"/src", "out", "more"},
		"unknown flag": {"--mkdir", "/src"},
	}
	for name, args := range cases {
		if _, _, err := parseGet(args); err == nil {
			t.Errorf("%s: parseGet(%v) = nil error", name, args)
		}
	}
}

// fakeAgent runs script on the server end of a pipe and returns the client
// end. The script plays the agent side of one control exchange.
func fakeAgent(t *testing.T, script func(conn net.Conn)) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	go func() {
		script(server)
		server.Close()
	}()
	return client
}

// readRequest consumes the opening request frame, as the agent would.
func readRequest(t *testing.T, conn net.Conn) vsockclient.Request {
	t.Helper()
	f, err := vsockclient.ReadFrame(conn)
	if err != nil {
		t.Errorf("read request: %v", err)
		return vsockclient.Request{}
	}
	var req vsockclient.Request
	if err := json.Unmarshal(f.Payload, &req); err != nil {
		t.Errorf("request json: %v", err)
	}
	return req
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestReadFramesWriteErrorFails(t *testing.T) {
	for name, w := range map[string]io.Writer{"error": failWriter{}, "short": shortWriter{}} {
		t.Run(name, func(t *testing.T) {
			conn := fakeAgent(t, func(c net.Conn) {
				vsockclient.WriteFrame(c, vsockclient.FrameStdout, []byte("abc"))
				vsockclient.WriteExit(c, 0)
			})
			defer conn.Close()

			code, _, err := readFrames(conn, w, false)

			if code != 1 || err == nil {
				t.Errorf("code = %d err = %v, want 1 and a write error", code, err)
			}
		})
	}
}

func TestDoGetFailureKeepsExistingDest(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(dest, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := fakeAgent(t, func(c net.Conn) {
		readRequest(t, c)
		vsockclient.WriteError(c, "get: open /nope: no such file or directory")
		vsockclient.WriteExit(c, 1)
	})
	defer conn.Close()

	code, err := doGet(conn, &vsockclient.Get{Path: "/nope"}, dest)

	if code != 1 || err != nil {
		t.Errorf("code = %d err = %v, want 1 and no local error", code, err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "keep me" {
		t.Errorf("dest = %q, want the original content untouched", got)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("dir has %d entries, want only out.txt (no temp file left)", len(ents))
	}
}

func TestDoGetTempCreateFailure(t *testing.T) {
	dir := t.TempDir()
	conn := fakeAgent(t, func(c net.Conn) { frameTypes(c) }) // never asked anything
	defer conn.Close()

	// A missing DEST directory makes the temp file creation fail before any
	// request is sent.
	code, err := doGet(conn, &vsockclient.Get{Path: "/f"}, filepath.Join(dir, "missing", "out"))

	if code != 1 || err == nil {
		t.Errorf("code = %d err = %v, want 1 and an error", code, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("dir entries = %v, want none", ents)
	}
}

func TestDoGetSuccessRenamesAndAppliesMode(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := fakeAgent(t, func(c net.Conn) {
		req := readRequest(t, c)
		if req.Op != vsockclient.OpGet || req.Get == nil || req.Get.Path != "/f" {
			t.Errorf("request = %+v, want get /f", req)
		}
		vsockclient.WriteFrame(c, vsockclient.FrameStdout, []byte("new "))
		vsockclient.WriteFrame(c, vsockclient.FrameStdout, []byte("content"))
		vsockclient.WriteFileInfo(c, vsockclient.FileInfo{Size: 11, Mode: 0o640})
		vsockclient.WriteExit(c, 0)
	})
	defer conn.Close()

	code, err := doGet(conn, &vsockclient.Get{Path: "/f"}, dest)

	if code != 0 || err != nil {
		t.Fatalf("code = %d err = %v, want 0 and nil", code, err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "new content" {
		t.Errorf("dest = %q, want %q", got, "new content")
	}
	if fi, _ := os.Stat(dest); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640 from file_info", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("dir has %d entries, want only out.txt", len(ents))
	}
}

// errReader yields data once, then a non-EOF error, like reading a directory.
type errReader struct {
	data string
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("read /srcdir: is a directory")
}

// frameTypes collects the types of every frame readable on conn until it fails.
func frameTypes(conn net.Conn) []vsockclient.FrameType {
	var types []vsockclient.FrameType
	for {
		f, err := vsockclient.ReadFrame(conn)
		if err != nil {
			return types
		}
		types = append(types, f.Type)
	}
}

func TestStreamInputReadErrorSendsNoEOF(t *testing.T) {
	client, server := net.Pipe()
	got := make(chan []vsockclient.FrameType, 1)
	go func() { got <- frameTypes(server) }()

	err := streamInput(client, &errReader{data: "abc"})
	client.Close()

	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("err = %v, want the reader's error", err)
	}
	if types := <-got; len(types) != 1 || types[0] != vsockclient.FrameStdin {
		t.Errorf("frames sent = %v, want only [stdin] and no stdin_eof", types)
	}
}

func TestStreamInputEOFSendsEOF(t *testing.T) {
	client, server := net.Pipe()
	got := make(chan []vsockclient.FrameType, 1)
	go func() { got <- frameTypes(server) }()

	err := streamInput(client, strings.NewReader("abc"))
	client.Close()

	if err != nil {
		t.Errorf("err = %v, want nil at EOF", err)
	}
	want := []vsockclient.FrameType{vsockclient.FrameStdin, vsockclient.FrameStdinEOF}
	if types := <-got; len(types) != 2 || types[0] != want[0] || types[1] != want[1] {
		t.Errorf("frames sent = %v, want %v", types, want)
	}
}

func TestDoPutReportsSourceReadError(t *testing.T) {
	var sawEOF bool
	conn := fakeAgent(t, func(c net.Conn) {
		readRequest(t, c)
		for _, ft := range frameTypes(c) {
			if ft == vsockclient.FrameStdinEOF {
				sawEOF = true
			}
		}
	})

	code, err := doPut(conn, &vsockclient.Put{Path: "/dst"}, &errReader{data: "abc"}, "/srcdir")

	if code != 1 || err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("code = %d err = %v, want 1 and the source read error", code, err)
	}
	if err != nil && strings.Contains(err.Error(), "connection closed") {
		t.Errorf("err = %v, should report the read error rather than the closed conn", err)
	}
	if sawEOF {
		t.Error("agent received stdin_eof after a source read error")
	}
}

func TestOpenSourceRejectsDirectory(t *testing.T) {
	_, err := openSource(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("err = %v, want an is a directory error", err)
	}
}

func TestOpenSourceStdin(t *testing.T) {
	r, err := openSource("-")
	if err != nil || r != os.Stdin {
		t.Errorf("r = %v err = %v, want os.Stdin", r, err)
	}
}

func TestPreparePutFileSourceSendsNoMode(t *testing.T) {
	src := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	p, in, _, err := preparePut([]string{src, "/opt/run.sh"})
	if err != nil {
		t.Fatalf("preparePut: %v", err)
	}
	defer in.Close()

	if p.Mode != 0 {
		t.Errorf("mode = %o, want 0 so the agent keeps an existing file's mode (or applies 0644)", p.Mode)
	}
	if p.Path != "/opt/run.sh" {
		t.Errorf("path = %q", p.Path)
	}
}

func TestPreparePutExplicitMode(t *testing.T) {
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, in, _, err := preparePut([]string{"--mode", "0755", src, "/dst"})
	if err != nil {
		t.Fatalf("preparePut: %v", err)
	}
	defer in.Close()

	if p.Mode != 0o755 {
		t.Errorf("mode = %o, want 755 from --mode", p.Mode)
	}
}
