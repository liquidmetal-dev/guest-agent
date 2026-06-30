// Package protocol defines the wire format spoken on the guest-agent control
// port: length-prefixed binary frames, full-duplex.
//
// Frame layout:
//
//	[1 byte type][4 byte big-endian length][length bytes payload]
//
// The first frame on a connection is always a Request frame carrying a JSON
// envelope (see messages.go). Subsequent frames stream stdin (host->agent) and
// stdout/stderr/exit/error (agent->host).
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FrameType identifies the kind of payload carried by a frame.
type FrameType uint8

const (
	// FrameRequest carries a JSON Request envelope (host -> agent, first frame).
	FrameRequest FrameType = iota + 1
	// FrameStdin carries raw bytes for the child's stdin (host -> agent).
	FrameStdin
	// FrameStdinEOF signals end of stdin; payload empty (host -> agent).
	FrameStdinEOF
	// FrameStdout carries raw bytes from the child's stdout (agent -> host).
	FrameStdout
	// FrameStderr carries raw bytes from the child's stderr (agent -> host).
	FrameStderr
	// FrameExit carries a JSON ExitMessage and ends the exchange (agent -> host).
	FrameExit
	// FrameError carries a JSON ErrorMessage; usually followed by FrameExit.
	FrameError
)

// MaxFrameSize caps a single frame's payload to guard against bad allocations
// from a corrupt or hostile length prefix.
const MaxFrameSize = 16 << 20 // 16 MiB

// ErrFrameTooLarge is returned when a frame's declared length exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("protocol: frame exceeds max size")

const headerSize = 5 // 1 type + 4 length

// Frame is a single decoded wire frame.
type Frame struct {
	Type    FrameType
	Payload []byte
}

// WriteFrame encodes one frame to w.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	var hdr [headerSize]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame decodes one frame from r. It returns io.EOF only when the reader is
// at a clean frame boundary; a truncated frame yields io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(hdr[1:])
	if length > MaxFrameSize {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, length)
	}
	f := Frame{Type: FrameType(hdr[0])}
	if length > 0 {
		f.Payload = make([]byte, length)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return Frame{}, err
		}
	}
	return f, nil
}

// String renders a human-readable frame type for logs.
func (t FrameType) String() string {
	switch t {
	case FrameRequest:
		return "request"
	case FrameStdin:
		return "stdin"
	case FrameStdinEOF:
		return "stdin_eof"
	case FrameStdout:
		return "stdout"
	case FrameStderr:
		return "stderr"
	case FrameExit:
		return "exit"
	case FrameError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}
