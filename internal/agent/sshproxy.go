package agent

import (
	"context"
	"io"
	"net"
)

// handleSSH proxies a raw control connection to the local sshd. It is a dumb
// byte pipe: sshd owns all authentication, PTY, and SFTP handling, so the
// system's existing host keys and user accounts apply unchanged.
func (a *Agent) handleSSH(ctx context.Context, conn net.Conn) {
	var d net.Dialer
	upstream, err := d.DialContext(ctx, "tcp", a.cfg.SSHTarget)
	if err != nil {
		// sshd down/absent: nothing framed to send on a raw port, so just
		// close. The host's ssh client sees a clean disconnect.
		a.log.Warn("ssh proxy dial failed", "target", a.cfg.SSHTarget, "error", err)
		return
	}
	defer upstream.Close()

	// Copy both directions; finish when either side closes.
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, upstream); done <- struct{}{} }()

	select {
	case <-done:
	case <-ctx.Done():
	}
}
