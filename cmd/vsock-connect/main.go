// Command vsock-connect is the host-side helper for talking to a guest-agent.
//
// Firecracker and Cloud Hypervisor expose the guest's vsock to the host as a
// Unix-domain socket multiplexer: to reach guest vsock port N you connect to
// the UDS, send "CONNECT N\n", read an "OK <hostport>\n" line, then stream raw
// bytes. This tool performs that handshake and then either:
//
//   - pipes stdio<->socket (raw mode), used as an ssh ProxyCommand; or
//   - speaks the control protocol (exec/put/get/ping/info modes).
//
// In TCP dev mode (--tcp host:port) it connects directly with no handshake, so
// the same tool drives a guest-agent started with --net tcp.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/liquidmetal-dev/guest-agent/pkg/vsockclient"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "exec":
		cmdExec(os.Args[2:])
	case "put":
		cmdPut(os.Args[2:])
	case "get":
		cmdGet(os.Args[2:])
	case "ping":
		cmdSimple(os.Args[2:], vsockclient.OpPing)
	case "info":
		cmdSimple(os.Args[2:], vsockclient.OpInfo)
	case "-h", "--help", "help":
		usage()
	default:
		cmdRaw(os.Args[1:]) // raw pipe mode (ssh ProxyCommand)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `vsock-connect — host helper for guest-agent

Connection (pick one):
  --uds PATH --port N    Firecracker/Cloud Hypervisor vsock UDS + CONNECT handshake
  --tcp HOST:PORT        direct TCP (guest-agent --net tcp, for dev)

Modes:
  vsock-connect [conn]                 raw stdio<->socket pipe (use as ssh ProxyCommand)
  vsock-connect exec [conn] [opts] -- CMD [ARGS...]   run a command, stream I/O
  vsock-connect put [conn] [opts] SRC DEST   copy a host file (or - for stdin) to DEST in the guest
  vsock-connect get [conn] [opts] SRC [DEST] copy a guest file to DEST on the host (default/-: stdout)
  vsock-connect ping [conn]            liveness check
  vsock-connect info [conn]            agent version + host info + supported ops

exec opts:
  --shell            run "sh -c" instead of direct exec
  --user U           run as system user U
  --timeout N        kill after N seconds (0 = unbounded)
  --stdin            stream this process's stdin to the command

put opts:
  --mode OCTAL       permission bits (default: SRC's mode; for stdin the agent's default)
  --owner U[:G]      owner as a name or numeric id (default: existing file's, else root)
  --mkdir            create missing parent directories
  --timeout N        abort after N seconds (0 = unbounded)

get opts:
  --timeout N        abort after N seconds (0 = unbounded)

Examples:
  vsock-connect exec --uds /run/flintlock/vm.vsock --port 1024 -- uname -a
  vsock-connect put --uds /run/flintlock/vm.vsock --port 1024 --mode 0755 run.sh /opt/app/run.sh
  vsock-connect get --uds /run/flintlock/vm.vsock --port 1024 /var/log/app.log app.log
  ssh -o ProxyCommand="vsock-connect --uds /run/flintlock/vm.vsock --port 1025" user@guest
`)
	os.Exit(2)
}

// connFlags holds the shared connection-target options.
type connFlags struct {
	uds  string
	port uint
	tcp  string
}

// parseConn pulls connection flags out of args, leaving the rest. It is a tiny
// hand-rolled parser so it can coexist with a "--" argv terminator.
func parseConn(args []string) (connFlags, []string, error) {
	var c connFlags
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			rest = append(rest, args[i:]...)
			return c, rest, nil
		case "--uds":
			i++
			v, err := arg(args, i)
			if err != nil {
				return c, nil, err
			}
			c.uds = v
		case "--port":
			i++
			v, err := arg(args, i)
			if err != nil {
				return c, nil, err
			}
			port, err := parseUintFlag("--port", v)
			if err != nil {
				return c, nil, err
			}
			c.port = port
		case "--tcp":
			i++
			v, err := arg(args, i)
			if err != nil {
				return c, nil, err
			}
			c.tcp = v
		default:
			rest = append(rest, args[i])
		}
	}
	return c, rest, nil
}

func arg(args []string, i int) (string, error) {
	if i >= len(args) {
		return "", fmt.Errorf("missing value for flag %q", args[i-1])
	}
	return args[i], nil
}

func parseUintFlag(name, value string) (uint, error) {
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid %s %q", name, value)
	}
	return uint(n), nil
}

// dial connects per the connection flags, performing the UDS handshake when
// needed, and returns a conn whose Read includes any bytes buffered during the
// handshake. The handshake and framing logic lives in pkg/vsockclient, shared
// with any other Go caller of the guest-agent (e.g. flintlock).
func dial(c connFlags) net.Conn {
	if c.tcp != "" {
		conn, err := vsockclient.DialTCP(context.Background(), c.tcp)
		if err != nil {
			fatal("%v", err)
		}
		return conn
	}
	if c.uds == "" || c.port == 0 {
		fatal("need --uds PATH --port N (or --tcp HOST:PORT)")
	}
	conn, err := vsockclient.Dial(context.Background(), c.uds, uint32(c.port))
	if err != nil {
		fatal("%v", err)
	}
	return conn
}

// cmdRaw pipes stdio to/from the socket. Used as an ssh ProxyCommand.
func cmdRaw(args []string) {
	c, rest, err := parseConn(args)
	if err != nil {
		fatal("%v", err)
	}
	if len(rest) > 0 {
		fatal("unexpected args: %v", rest)
	}
	conn := dial(c)
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, os.Stdin); done <- struct{}{} }()
	go func() { io.Copy(os.Stdout, conn); done <- struct{}{} }()
	<-done
}

// cmdExec runs a command on the guest and mirrors its I/O and exit code.
func cmdExec(args []string) {
	c, rest, err := parseConn(args)
	if err != nil {
		fatal("%v", err)
	}
	e, err := parseExec(rest)
	if err != nil {
		fatal("%v", err)
	}

	conn := dial(c)
	defer conn.Close()

	req := &vsockclient.Request{Version: vsockclient.Version, Op: vsockclient.OpExec, Exec: e}
	if err := vsockclient.WriteRequest(conn, req); err != nil {
		fatal("send request: %v", err)
	}
	if e.HasStdin {
		go streamInput(conn, os.Stdin)
	}
	os.Exit(readResponses(conn, false))
}

// cmdPut copies a host file (or stdin) into the guest.
func cmdPut(args []string) {
	c, rest, err := parseConn(args)
	if err != nil {
		fatal("%v", err)
	}
	p, src, err := parsePut(rest)
	if err != nil {
		fatal("%v", err)
	}

	in := os.Stdin
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			fatal("%v", err)
		}
		defer f.Close()
		if p.Mode == 0 {
			fi, err := f.Stat()
			if err != nil {
				fatal("%v", err)
			}
			p.Mode = uint32(fi.Mode().Perm())
		}
		in = f
	}

	conn := dial(c)
	defer conn.Close()

	req := &vsockclient.Request{Version: vsockclient.Version, Op: vsockclient.OpPut, Put: p}
	if err := vsockclient.WriteRequest(conn, req); err != nil {
		fatal("send request: %v", err)
	}
	go streamInput(conn, in)
	code, _, err := readFrames(conn, io.Discard, false)
	if err != nil {
		fatal("%v", err)
	}
	os.Exit(code)
}

// parsePut parses put options and returns the request plus the host source
// path ("-" for stdin).
func parsePut(args []string) (*vsockclient.Put, string, error) {
	p := &vsockclient.Put{}
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--mode":
			i++
			v, err := arg(args, i)
			if err != nil {
				return nil, "", err
			}
			mode, err := strconv.ParseUint(v, 8, 32)
			if err != nil || mode > 0o7777 {
				return nil, "", fmt.Errorf("invalid --mode %q (want octal permission bits)", v)
			}
			p.Mode = uint32(mode)
		case "--owner":
			i++
			v, err := arg(args, i)
			if err != nil {
				return nil, "", err
			}
			p.User, p.Group, _ = strings.Cut(v, ":")
		case "--mkdir":
			p.MkdirParents = true
		case "--timeout":
			i++
			v, err := arg(args, i)
			if err != nil {
				return nil, "", err
			}
			timeout, err := parseTimeout(v)
			if err != nil {
				return nil, "", err
			}
			p.TimeoutSec = timeout
		default:
			if strings.HasPrefix(args[i], "-") && args[i] != "-" {
				return nil, "", fmt.Errorf("unknown put option %q", args[i])
			}
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 2 {
		return nil, "", fmt.Errorf("use: put [opts] SRC DEST (SRC may be - for stdin)")
	}
	if !filepath.IsAbs(pos[1]) {
		return nil, "", fmt.Errorf("DEST %q must be an absolute path in the guest", pos[1])
	}
	p.Path = pos[1]
	return p, pos[0], nil
}

// cmdGet copies a guest file to a host file or stdout.
func cmdGet(args []string) {
	c, rest, err := parseConn(args)
	if err != nil {
		fatal("%v", err)
	}
	g, dest, err := parseGet(rest)
	if err != nil {
		fatal("%v", err)
	}

	conn := dial(c)
	defer conn.Close()
	code, err := doGet(conn, g, dest)
	if err != nil {
		fatal("%v", err)
	}
	os.Exit(code)
}

// doGet sends the get request on conn and streams the reply to dest ("" or
// "-" for stdout). A file dest is received into a temp file beside it and
// renamed into place only after a successful transfer, so a failed get never
// touches an existing dest. The returned code is the agent's exit code; a
// non-nil error is a host-side failure (and then code is 1).
func doGet(conn net.Conn, g *vsockclient.Get, dest string) (int, error) {
	if dest == "" || dest == "-" {
		if err := vsockclient.WriteRequest(conn, &vsockclient.Request{Version: vsockclient.Version, Op: vsockclient.OpGet, Get: g}); err != nil {
			return 1, fmt.Errorf("send request: %w", err)
		}
		code, _, err := readFrames(conn, os.Stdout, false)
		return code, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*")
	if err != nil {
		return 1, err
	}
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmp.Name())
		}
	}()

	if err := vsockclient.WriteRequest(conn, &vsockclient.Request{Version: vsockclient.Version, Op: vsockclient.OpGet, Get: g}); err != nil {
		return 1, fmt.Errorf("send request: %w", err)
	}
	code, info, err := readFrames(conn, tmp, false)
	if err != nil || code != 0 {
		return code, err
	}
	if info != nil {
		if err := tmp.Chmod(os.FileMode(info.Mode & 0o777)); err != nil {
			return 1, err
		}
	}
	if err := tmp.Close(); err != nil {
		return 1, err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return 1, err
	}
	committed = true
	return 0, nil
}

// parseGet parses get options and returns the request plus the host
// destination ("" or "-" for stdout).
func parseGet(args []string) (*vsockclient.Get, string, error) {
	g := &vsockclient.Get{}
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--timeout":
			i++
			v, err := arg(args, i)
			if err != nil {
				return nil, "", err
			}
			timeout, err := parseTimeout(v)
			if err != nil {
				return nil, "", err
			}
			g.TimeoutSec = timeout
		default:
			if strings.HasPrefix(args[i], "-") && args[i] != "-" {
				return nil, "", fmt.Errorf("unknown get option %q", args[i])
			}
			pos = append(pos, args[i])
		}
	}
	if len(pos) < 1 || len(pos) > 2 {
		return nil, "", fmt.Errorf("use: get [opts] SRC [DEST] (DEST defaults to stdout)")
	}
	if !filepath.IsAbs(pos[0]) {
		return nil, "", fmt.Errorf("SRC %q must be an absolute path in the guest", pos[0])
	}
	g.Path = pos[0]
	if len(pos) == 2 {
		return g, pos[1], nil
	}
	return g, "", nil
}

func parseTimeout(v string) (int, error) {
	timeout, err := strconv.Atoi(v)
	if err != nil || timeout < 0 {
		return 0, fmt.Errorf("invalid --timeout %q", v)
	}
	return timeout, nil
}

func parseExec(args []string) (*vsockclient.Exec, error) {
	e := &vsockclient.Exec{}
	var argv []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--shell":
			e.Shell = true
		case "--stdin":
			e.HasStdin = true
		case "--user":
			i++
			v, err := arg(args, i)
			if err != nil {
				return nil, err
			}
			e.User = v
		case "--timeout":
			i++
			v, err := arg(args, i)
			if err != nil {
				return nil, err
			}
			timeout, err := parseTimeout(v)
			if err != nil {
				return nil, err
			}
			e.TimeoutSec = timeout
		case "--":
			argv = args[i+1:]
			i = len(args)
		default:
			return nil, fmt.Errorf("unknown exec option %q (did you forget \"--\" before the command?)", args[i])
		}
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("no command given; use: exec [opts] -- CMD [ARGS...]")
	}
	e.Cmd, e.Args = argv[0], argv[1:]
	return e, nil
}

// cmdSimple runs a no-payload op (ping/info) and reports the result.
func cmdSimple(args []string, op vsockclient.Op) {
	c, rest, err := parseConn(args)
	if err != nil {
		fatal("%v", err)
	}
	if len(rest) > 0 {
		fatal("unexpected args: %v", rest)
	}
	conn := dial(c)
	defer conn.Close()
	req := &vsockclient.Request{Version: vsockclient.Version, Op: op}
	if err := vsockclient.WriteRequest(conn, req); err != nil {
		fatal("send request: %v", err)
	}
	os.Exit(readResponses(conn, op == vsockclient.OpInfo))
}

// streamInput forwards r to the agent as stdin frames, then a stdin_eof frame.
func streamInput(conn net.Conn, r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := vsockclient.WriteFrame(conn, vsockclient.FrameStdin, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			vsockclient.WriteFrame(conn, vsockclient.FrameStdinEOF, nil)
			return
		}
	}
}

// readResponses consumes agent frames until exit, writing stdout/stderr through
// and returning the exit code. When infoMode, a stdout frame is reprinted as-is
// (it carries the InfoMessage JSON).
func readResponses(conn net.Conn, infoMode bool) int {
	code, _, err := readFrames(conn, os.Stdout, infoMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vsock-connect: %v\n", err)
	}
	return code
}

// readFrames is readResponses with stdout frames sent to w. It also returns
// the file_info frame of a put/get, if one arrived. A conn read failure or a
// failed or short write to w ends the exchange with code 1 and that error.
func readFrames(conn net.Conn, w io.Writer, infoMode bool) (int, *vsockclient.FileInfo, error) {
	var info *vsockclient.FileInfo
	for {
		f, err := vsockclient.ReadFrame(conn)
		if err != nil {
			return 1, info, fmt.Errorf("connection closed: %w", err)
		}
		switch f.Type {
		case vsockclient.FrameStdout:
			if infoMode {
				f.Payload = append(f.Payload, '\n')
			}
			if err := writeAll(w, f.Payload); err != nil {
				return 1, info, err
			}
		case vsockclient.FrameFileInfo:
			var fi vsockclient.FileInfo
			if json.Unmarshal(f.Payload, &fi) == nil {
				info = &fi
			}
		case vsockclient.FrameStderr:
			os.Stderr.Write(f.Payload)
		case vsockclient.FrameError:
			var em vsockclient.ErrorMessage
			if json.Unmarshal(f.Payload, &em) == nil {
				fmt.Fprintf(os.Stderr, "vsock-connect: agent error: %s\n", em.Msg)
			} else {
				fmt.Fprintf(os.Stderr, "vsock-connect: agent error: %s\n", f.Payload)
			}
		case vsockclient.FrameExit:
			var ex vsockclient.ExitMessage
			_ = json.Unmarshal(f.Payload, &ex)
			return ex.Code, info, nil
		}
	}
}

// writeAll writes p to w, treating a short write as an error.
func writeAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "vsock-connect: "+format+"\n", a...)
	os.Exit(1)
}
