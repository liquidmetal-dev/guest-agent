# AGENTS.md

Guide for AI agents and contributors working in this repository.

## Project

`guest-agent` is a vsock-based agent that runs inside a microVM guest, plus
`vsock-connect`, a host-side helper that dials the guest over vsock. Communication
uses a small framed protocol; the agent supports command execution and an SSH proxy.

## Layout

- `cmd/guest-agent/` — guest binary (`main`).
- `cmd/vsock-connect/` — host helper binary (`main`).
- `internal/agent/` — agent core: control loop, exec, SSH proxy. `Version` is set here.
- `internal/protocol/` — frame + message wire format.
- `internal/transport/` — vsock/tcp transport.
- `test/e2e/` — Cloud Hypervisor end-to-end test (`//go:build e2e`) and helpers.
- `init/` — systemd unit. `docs/` — host usage docs.

## Build

```sh
make build     # both binaries into bin/ (CGO disabled, static)
make host      # vsock-connect only
make release   # cross-compile linux/amd64 and linux/arm64
```

Version is injected via ldflags into `github.com/liquidmetal-dev/guest-agent/internal/agent.Version`.

## Test

```sh
make test      # unit tests: go test ./...
make e2e       # Cloud Hypervisor e2e — requires /dev/kvm + cloud-hypervisor
```

E2E tests are gated behind `//go:build e2e`, so `go test ./...` never compiles or
runs them and needs no KVM.

## Lint / format

```sh
golangci-lint run   # config in .golangci.yml
make fmt            # gofmt -w cmd internal
make vet            # go vet ./...
```

## Conventions

- Go 1.26 (keep `setup-go` versions in CI in sync with `toolchain` in `go.mod`).
- `CGO_ENABLED=0`; binaries are static.
- Keep new code gofmt-clean and vet-clean; run `golangci-lint run` before pushing.

## CI

- `.github/workflows/ci.yml` — runs on PRs: `build`, `lint`, `test` jobs.
- `.github/workflows/e2e.yml` — runs the Cloud Hypervisor e2e test on PRs.
- `.github/workflows/release.yml` — on `v*` tag push, runs GoReleaser
  (`.goreleaser.yaml`) to publish archives, `.deb`/`.rpm`, checksums, and a changelog
  to the GitHub Release. Keep its `setup-go` version in sync with `go.mod` too.
- Dependency updates: `.github/dependabot.yml` (weekly, grouped per ecosystem).
