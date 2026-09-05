// Command vsock-connect is the host-side helper for talking to a guest-agent.
//
// Firecracker and Cloud Hypervisor expose the guest's vsock to the host as a
// Unix-domain socket multiplexer: to reach guest vsock port N you connect to
// the UDS, send "CONNECT N\n", read an "OK <hostport>\n" line, then stream raw
// bytes. This tool performs that handshake and then either:
//
//   - pipes stdio<->socket (raw mode), used as an ssh ProxyCommand; or
//   - speaks the control protocol (exec/ping/info modes).
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
	"strconv"

	"github.com/liquidmetal-dev/guest-agent/pkg/vsockclient"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "exec":
		cmdExec(os.Args[2:])
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
  vsock-connect ping [conn]            liveness check
  vsock-connect info [conn]            agent version + host info

exec opts:
  --shell            run "sh -c" instead of direct exec
  --user U           run as system user U
  --timeout N        kill after N seconds (0 = unbounded)
  --stdin            stream this process's stdin to the command

Examples:
  vsock-connect exec --uds /run/flintlock/vm.vsock --port 1024 -- uname -a
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
		go streamStdin(conn)
	}
	os.Exit(readResponses(conn, false))
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
			timeout, err := strconv.Atoi(v)
			if err != nil || timeout < 0 {
				return nil, fmt.Errorf("invalid --timeout %q", v)
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

// streamStdin forwards this process's stdin to the command as stdin frames.
func streamStdin(conn net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := os.Stdin.Read(buf)
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
	for {
		f, err := vsockclient.ReadFrame(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vsock-connect: connection closed: %v\n", err)
			return 1
		}
		switch f.Type {
		case vsockclient.FrameStdout:
			os.Stdout.Write(f.Payload)
			if infoMode {
				os.Stdout.Write([]byte("\n"))
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
			return ex.Code
		}
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "vsock-connect: "+format+"\n", a...)
	os.Exit(1)
}
