# CryoBackup build entry points. Everything is plain `go`; no code generation yet.
MODULE   := github.com/c2tech-sys/cryobackup
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X $(MODULE)/internal/buildinfo.Version=$(VERSION) -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) -X $(MODULE)/internal/buildinfo.Date=$(DATE)
BIN      := bin

.PHONY: all build test vet lint cross clean tidy

all: vet test build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/...

test:
	go test -race -count=1 -timeout 30m ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

# Windows agent and Linux console from any host.
cross:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/windows_amd64/cryo-agent.exe ./cmd/cryo-agent
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/linux_amd64/cryo-console ./cmd/cryo-console

clean:
	rm -rf $(BIN)

# Windows agent MSI (Windows host; deploy/windows/README.md). Needs Go, MinGW-w64
# gcc, the .NET SDK (WiX 5.0.2 comes from .config/dotnet-tools.json) and
# CRYO_RESTIC_EXE pointing at the pinned restic.exe (deploy/windows/fetch-restic.ps1).
# Signs only when CRYO_SIGN_COMMAND is set.
.PHONY: msi
msi:
	powershell -NoProfile -ExecutionPolicy Bypass -File deploy/windows/build.ps1
