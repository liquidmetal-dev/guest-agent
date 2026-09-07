// Package vsockclient is the public, importable half of the guest-agent
// control-channel client: the UDS CONNECT handshake used to reach a guest's
// vsock through Firecracker/Cloud Hypervisor, and the wire types/framing used
// to speak to the agent once connected.
//
// cmd/vsock-connect is the CLI built on top of this package; anything that
// wants to drive a guest-agent from Go (rather than shelling out to that CLI)
// should import this package instead of reimplementing the protocol.
package vsockclient

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/liquidmetal-dev/guest-agent/internal/protocol"
)

// HandshakeTimeout bounds the UDS CONNECT handshake so a not-yet-ready (or
// never-ready) guest agent fails fast instead of blocking forever.
const HandshakeTimeout = 10 * time.Second

// Protocol version and control-channel operations.
const (
	Version = protocol.Version

	OpExec Op = protocol.OpExec
	OpPing Op = protocol.OpPing
	OpInfo Op = protocol.OpInfo
)

// Frame types carried on the control channel.
const (
	FrameRequest  FrameType = protocol.FrameRequest
	FrameStdin    FrameType = protocol.FrameStdin
	FrameStdinEOF FrameType = protocol.FrameStdinEOF
	FrameStdout   FrameType = protocol.FrameStdout
	FrameStderr   FrameType = protocol.FrameStderr
	FrameExit     FrameType = protocol.FrameExit
	FrameError    FrameType = protocol.FrameError

	MaxFrameSize = protocol.MaxFrameSize
)

// Type aliases onto the internal protocol package, so callers work with these
// names without this package having to re-declare or convert between them.
type (
	Op           = protocol.Op
	FrameType    = protocol.FrameType
	Frame        = protocol.Frame
	Request      = protocol.Request
	Exec         = protocol.Exec
	ExitMessage  = protocol.ExitMessage
	ErrorMessage = protocol.ErrorMessage
	InfoMessage  = protocol.InfoMessage
)

// ErrFrameTooLarge is returned when a frame's declared length exceeds MaxFrameSize.
var ErrFrameTooLarge = protocol.ErrFrameTooLarge

// WriteRequest encodes req into a FrameRequest on w.
func WriteRequest(w interface{ Write([]byte) (int, error) }, req *Request) error {
	return protocol.WriteRequest(w, req)
}

// WriteExit encodes an exit code as a FrameExit on w.
func WriteExit(w interface{ Write([]byte) (int, error) }, code int) error {
	return protocol.WriteExit(w, code)
}

// WriteError encodes msg as a FrameError on w.
func WriteError(w interface{ Write([]byte) (int, error) }, msg string) error {
	return protocol.WriteError(w, msg)
}

// WriteFrame encodes one frame to w.
func WriteFrame(w interface{ Write([]byte) (int, error) }, t FrameType, payload []byte) error {
	return protocol.WriteFrame(w, t, payload)
}

// ReadFrame decodes one frame from r.
func ReadFrame(r interface{ Read([]byte) (int, error) }) (Frame, error) {
	return protocol.ReadFrame(r)
}

// Dial connects to a guest-agent's control channel exposed as a vsock port
// behind a Firecracker/Cloud Hypervisor UDS multiplexer at udsPath: it opens
// the UDS, performs the "CONNECT <port>\n" / "OK <hostport>\n" handshake, and
// returns a net.Conn ready to carry the framed protocol (see WriteRequest,
// ReadFrame). The handshake is bounded by HandshakeTimeout or ctx, whichever
// fires first; once established the returned conn has no deadline.
func Dial(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	deadline := time.Now().Add(HandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial uds %s: %w", udsPath, err)
	}

	wrapped, err := handshake(conn, port, deadline)
	if err != nil {
		conn.Close()
		return nil, err
	}

	return wrapped, nil
}

// DialTCP connects directly to addr with no handshake, for use against a
// guest-agent started with --net tcp (local development and tests).
func DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %w", addr, err)
	}

	return conn, nil
}

// handshake performs the CONNECT/OK exchange on conn and returns a net.Conn
// whose Read comes from the buffered reader used to read the handshake
// reply, so no bytes read ahead during the handshake are lost.
func handshake(conn net.Conn, port uint32, deadline time.Time) (net.Conn, error) {
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return nil, fmt.Errorf("handshake write: %w", err)
	}

	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("handshake read: %w", err)
	}
	if fields := strings.Fields(line); len(fields) == 0 || fields[0] != "OK" {
		return nil, fmt.Errorf("handshake rejected: %q", strings.TrimSpace(line))
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear handshake deadline: %w", err)
	}

	return &bufConn{Conn: conn, r: r}, nil
}

// bufConn reads through a bufio.Reader (carrying post-handshake bytes) while
// writing straight to the underlying conn.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// CloseWrite half-closes the write side, signalling EOF to the peer while
// still allowing reads - used to tell the guest-agent "no more input" without
// tearing down the whole connection. Both the production UDS path
// (*net.UnixConn) and the --net tcp dev path (*net.TCPConn) support this; it
// returns an error if the underlying conn doesn't.
func (b *bufConn) CloseWrite() error {
	cw, ok := b.Conn.(interface{ CloseWrite() error })
	if !ok {
		return fmt.Errorf("%T does not support half-close", b.Conn)
	}

	return cw.CloseWrite()
}
