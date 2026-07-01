# Host usage

The guest runs `guest-agent` (listening on `AF_VSOCK`). The host drives it with the
`vsock-connect` helper. This document covers the production vsock path and the TCP dev
path.

## Background: vsock through Firecracker / Cloud Hypervisor

Inside the guest, vsock is normal `AF_VSOCK`. On the **host**, both Firecracker and Cloud
Hypervisor expose the guest's vsock as a single **Unix-domain socket multiplexer**. To
reach guest vsock port `N` you:

1. connect to the host UDS path configured for that VM (e.g. `/run/flintlock/<vm>.vsock`);
2. write `CONNECT <N>\n`;
3. read the `OK <hostport>\n` reply line;
4. stream raw bytes.

`vsock-connect` performs this handshake for you. (Guest→host connections use per-port UDS
paths `<uds>_<port>` — not needed here, since the host always initiates.)

The guest is addressed by its **CID**, configured in the VMM / flintlock spec — not by
this agent.

Default ports: control `1024`, ssh `1025`.

## Run a command

```sh
# argv-direct (no shell)
vsock-connect exec --uds /run/flintlock/vm.vsock --port 1024 -- uname -a

# shell features (pipes, globs, redirects)
vsock-connect exec --uds /run/flintlock/vm.vsock --port 1024 --shell -- 'dmesg | tail -5'

# run as a specific user
vsock-connect exec --uds /run/flintlock/vm.vsock --port 1024 --user ubuntu -- id

# bound the run
vsock-connect exec --uds /run/flintlock/vm.vsock --port 1024 --timeout 10 -- sleep 30

# stream stdin into the command (push a file through tee)
vsock-connect exec --uds /run/flintlock/vm.vsock --port 1024 --stdin -- tee /etc/motd < motd.txt
```

stdout/stderr stream back separately and the command's exit code becomes
`vsock-connect`'s own exit code, so it composes in scripts:

```sh
if vsock-connect exec --uds /run/flintlock/vm.vsock --port 1024 -- test -f /done; then
  echo "guest is ready"
fi
```

Interactive programs (`top`, `vim`) are **not** for `exec` (no PTY) — use the ssh path.

## Liveness and inventory

```sh
vsock-connect ping --uds /run/flintlock/vm.vsock --port 1024   # exit 0 if agent alive
vsock-connect info --uds /run/flintlock/vm.vsock --port 1024   # version + uname + uptime
```

## SSH over vsock

The agent proxies the ssh port straight to the guest's local sshd (`127.0.0.1:22`), so the
guest's existing users and host keys apply. Use `vsock-connect` (raw mode) as the ssh
`ProxyCommand`:

```sh
ssh -o ProxyCommand="vsock-connect --uds /run/flintlock/vm.vsock --port 1025" ubuntu@guest
```

Or in `~/.ssh/config`:

```
Host myvm
    User ubuntu
    ProxyCommand vsock-connect --uds /run/flintlock/vm.vsock --port 1025
```

The guest must be running sshd on `--ssh-target` (default `127.0.0.1:22`); the agent does
not manage sshd.

## TCP dev mode (no VM)

Start the agent with `--net tcp` and reach it with `--tcp HOST:PORT` instead of
`--uds/--port` (no handshake):

```sh
# terminal 1 — guest-agent on loopback
guest-agent --net tcp --control-port 1024 --ssh-port 1025

# terminal 2
vsock-connect exec --tcp 127.0.0.1:1024 -- echo hello
ssh -o ProxyCommand="vsock-connect --tcp 127.0.0.1:1025" "$USER"@127.0.0.1
```
