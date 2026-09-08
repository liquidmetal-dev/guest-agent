VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/liquidmetal-dev/guest-agent/internal/agent.Version=$(VERSION)
GOFLAGS := CGO_ENABLED=0

BINDIR := bin
PLATFORMS := linux/amd64 linux/arm64

.PHONY: all build host test e2e vet fmt lint tools clean release

all: build

# Build both binaries for the host arch.
build:
	$(GOFLAGS) go build -ldflags '$(LDFLAGS)' -o $(BINDIR)/guest-agent ./cmd/guest-agent
	$(GOFLAGS) go build -ldflags '$(LDFLAGS)' -o $(BINDIR)/vsock-connect ./cmd/vsock-connect

# Build only the host helper.
host:
	$(GOFLAGS) go build -ldflags '$(LDFLAGS)' -o $(BINDIR)/vsock-connect ./cmd/vsock-connect

test:
	go test ./...

e2e:
	go test -tags e2e ./test/e2e -run TestCloudHypervisorAgentE2E -timeout 3m -v

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

lint: vet
	@files="$$(gofmt -l cmd internal)"; \
	if [ -n "$$files" ]; then \
		printf '%s\n' "$$files"; \
		exit 1; \
	fi
	golangci-lint run

# Run once (or after mise.toml changes) to install pinned Go/golangci-lint/goreleaser via mise.
tools:
	@command -v mise >/dev/null 2>&1 || { echo "mise not found: https://mise.jdx.dev/getting-started.html"; exit 1; }
	mise install

# Static multi-arch release builds.
release:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		$(GOFLAGS) GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' \
			-o $(BINDIR)/guest-agent-$$os-$$arch ./cmd/guest-agent; \
		$(GOFLAGS) GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' \
			-o $(BINDIR)/vsock-connect-$$os-$$arch ./cmd/vsock-connect; \
	done

clean:
	rm -rf $(BINDIR)
