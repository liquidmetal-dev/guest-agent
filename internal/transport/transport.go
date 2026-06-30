// Package transport provides the listening sockets the agent accepts on. Two
// backends implement the same net.Listener-shaped contract: vsock for
// production (inside a VM) and tcp for local development and CI where no
// hypervisor vsock device is available.
package transport

import (
	"fmt"
	"net"

	"github.com/mdlayher/vsock"
)

// Kind selects a transport backend.
type Kind string

const (
	// VSOCK binds AF_VSOCK on the given port (production, inside a guest VM).
	VSOCK Kind = "vsock"
	// TCP binds TCP on Addr:port (development / CI, no VM required).
	TCP Kind = "tcp"
)

// Config parameterises Listen.
type Config struct {
	Kind Kind
	// Addr is the bind host for the TCP backend (ignored for vsock). Empty
	// defaults to 127.0.0.1 so dev mode never accidentally faces the network.
	Addr string
}

// Listen opens a listener of the configured kind on port. For vsock it binds
// VMADDR_CID_ANY so the host can reach it via the guest's CID.
func Listen(cfg Config, port uint32) (net.Listener, error) {
	switch cfg.Kind {
	case VSOCK:
		l, err := vsock.Listen(port, nil)
		if err != nil {
			return nil, fmt.Errorf("vsock listen on port %d: %w", port, err)
		}
		return l, nil
	case TCP:
		host := cfg.Addr
		if host == "" {
			host = "127.0.0.1"
		}
		addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("tcp listen on %s: %w", addr, err)
		}
		return l, nil
	default:
		return nil, fmt.Errorf("unknown transport kind %q", cfg.Kind)
	}
}
