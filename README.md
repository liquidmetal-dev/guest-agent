# guest-agent

A small agent that installs into a guest VM and lets the host control it over
**vsock** — no guest network required. The host can:

- **run commands** on the guest, with streamed stdin/stdout/stderr and faithful exit codes;
- **SSH into the guest over vsock**, proxied to the guest's local sshd.

Built for the [LiquidMetal](https://github.com/liquidmetal-dev) stack (flintlock +
Firecracker / Cloud Hypervisor), where vsock is surfaced to the host as a Unix-domain
socket multiplexer.

## Components

| Binary | Runs on | Purpose |
|---|---|---|
| `guest-agent` | guest VM | listens on `AF_VSOCK`; control port (exec/ping/info) + ssh proxy |
| `vsock-connect` | host | does the UDS `CONNECT` handshake; ssh `ProxyCommand` + exec client |

## How it works

`guest-agent` opens two vsock listeners:

- **control** (default port `1024`) — a length-prefixed binary protocol. The host sends a
  `request` frame (`exec` / `ping` / `info`); for `exec` it then streams `stdin` frames,
  and the agent streams back `stdout` / `stderr` / `exit`. Commands run in their own
  process group and are SIGTERM→SIGKILL reaped on timeout or host disconnect — no orphans.
- **ssh** (default port `1025`) — a raw byte proxy to `127.0.0.1:22`. sshd owns all auth,
  PTY, and SFTP; the agent is a dumb pipe.

Security model: vsock is reachable only from the host hypervisor, so the host **is** the
trust boundary — there is no additional auth on the control channel.

## Build

```sh
make build      # bin/guest-agent + bin/vsock-connect (static, CGO off)
make release    # linux/amd64 + linux/arm64 static binaries
make test
```

## Quickstart (dev, no VM)

```sh
make build
./bin/guest-agent --net tcp --control-port 1024 --ssh-port 1025 &
./bin/vsock-connect exec --tcp 127.0.0.1:1024 -- uname -a
```

## Install in a VM

1. Copy `bin/guest-agent` to `/usr/local/bin/guest-agent` in the guest image.
2. Install `init/guest-agent.service` and `systemctl enable --now guest-agent`.
3. From the host, drive it with `vsock-connect` — see [docs/host-usage.md](docs/host-usage.md).

## Configuration

Flags (each mirrored by a `GA_*` env var):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--net` | `GA_NET` | `vsock` | transport: `vsock` or `tcp` (dev) |
| `--control-port` | `GA_CONTROL_PORT` | `1024` | control listener port |
| `--ssh-port` | `GA_SSH_PORT` | `1025` | ssh proxy listener port |
| `--ssh-target` | `GA_SSH_TARGET` | `127.0.0.1:22` | local sshd address |
| `--tcp-addr` | `GA_TCP_ADDR` | `127.0.0.1` | bind host for `--net tcp` |
| `--log-level` | `GA_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `--log-format` | `GA_LOG_FORMAT` | `text` | `text` or `json` |
