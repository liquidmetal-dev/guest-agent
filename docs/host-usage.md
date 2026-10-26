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

## Copy files

```sh
# copy a host file into the guest (mode copied from the host file)
vsock-connect put --uds /run/flintlock/vm.vsock --port 1024 run.sh /opt/app/run.sh

# write from stdin, with an explicit mode and owner (name or numeric id)
echo "token" | vsock-connect put --uds /run/flintlock/vm.vsock --port 1024 --mode 0600 --owner ubuntu - /home/ubuntu/.token

# create missing parent directories (0755, same owner as the file)
vsock-connect put --uds /run/flintlock/vm.vsock --port 1024 --mkdir main.py /work/src/main.py

# read a guest file to stdout
vsock-connect get --uds /run/flintlock/vm.vsock --port 1024 /var/log/app.log

# read a guest file into a host file (mode preserved)
vsock-connect get --uds /run/flintlock/vm.vsock --port 1024 /work/out/result.json result.json
```

`put` writes to a temporary file beside the destination and renames it into place, so a
reader in the guest never sees a partial file. Without `--mode` or `--owner`, an existing
file keeps its mode and owner; a new file gets `0644` owned by root. Paths in the guest
must be absolute. `get` serves regular files only; copy a directory with
`exec -- tar` instead. `--timeout` bounds the whole transfer, as it does for `exec`.

Both commands exit `0` on success. Any failure prints the agent's error (which includes
the OS error text, e.g. `no such file or directory`) and exits `1`. `get` into a host file
receives into a temporary file beside it and renames on success, so a failed `get` leaves
an existing destination untouched.

Agents older than the file operations answer `put` and `get` with `unknown op` and exit
`1`. Check the `ops` list in `info` before relying on them.

## Liveness and inventory

```sh
vsock-connect ping --uds /run/flintlock/vm.vsock --port 1024   # exit 0 if agent alive
vsock-connect info --uds /run/flintlock/vm.vsock --port 1024   # version + uname + uptime + ops
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
echo hi | vsock-connect put --tcp 127.0.0.1:1024 - /tmp/hi && vsock-connect get --tcp 127.0.0.1:1024 /tmp/hi
ssh -o ProxyCommand="vsock-connect --tcp 127.0.0.1:1025" "$USER"@127.0.0.1
```
