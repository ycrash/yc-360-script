# Build and Test Guide

This guide covers how to build the `yc` binary for Linux (both `amd64` and `arm64`) using Dockerfiles, and how to run the Go test suite.

---

## Prerequisites

- [Docker Desktop](https://www.docker.com/products/docker-desktop/) with **BuildKit / buildx** enabled
- Go 1.25.8 or later (required only for running tests locally without Docker)

Verify buildx is available:

```sh
docker buildx ls
```

---

## Building the yc Binary for Linux

Two Dockerfiles are provided:

| File | Purpose |
|---|---|
| `Dockerfile.base.alpine` | Multi-arch build with `buildx` — exports only the `yc` binary (recommended) |
| `Dockerfile.linux` | Single-arch build using `docker buildx build --target final` |

### Option 1 — Multi-arch build via Makefile (Recommended)

This single command builds both `linux/amd64` and `linux/arm64` binaries and places them under `bin/linux/`:

```sh
make build-all
```

Output:

```
bin/
└── linux/
    ├── amd64/
    │   └── yc
    └── arm64/
        └── yc
```

Internally, `make build-all` runs the following two `docker buildx build` commands:

**linux/amd64:**

```sh
docker buildx build \
    --platform linux/amd64 \
    -f Dockerfile.base.alpine \
    --target export \
    --output type=local,dest=bin/linux/amd64 \
    .
```

**linux/arm64:**

```sh
docker buildx build \
    --platform linux/arm64 \
    -f Dockerfile.base.alpine \
    --target export \
    --output type=local,dest=bin/linux/arm64 \
    .
```

### Option 2 — Build using Dockerfile.linux directly

Use this when you want an explicit single-arch build via `Dockerfile.linux`.

**linux/amd64:**

```sh
docker buildx build \
    --platform linux/amd64 \
    -f Dockerfile.linux \
    --target final \
    --output type=local,dest=bin/linux/amd64 \
    .
```

**linux/arm64:**

```sh
docker buildx build \
    --platform linux/arm64 \
    -f Dockerfile.linux \
    --target final \
    --output type=local,dest=bin/linux/arm64 \
    .
```

### Option 3 — Interactive containerized build (single arch)

Use this for iterative development or debugging inside the build container.

```sh
# Step 1: Build the base image and start the container
make base

# Step 2: Open a shell inside the container
make shell

# Step 3: Build inside the container (run this inside the shell)
make build
```

The binary is written to `bin/yc`.

### Verifying the Binaries

After any of the above build options, verify the output:

```sh
file bin/linux/amd64/yc
# bin/linux/amd64/yc: ELF 64-bit LSB executable, x86-64, statically linked, stripped

file bin/linux/arm64/yc
# bin/linux/arm64/yc: ELF 64-bit LSB executable, ARM aarch64, statically linked, stripped
```

---

## Running the Go Tests

Tests are run directly on the host using the standard Go toolchain. No Docker is required.

### Run all tests

```sh
go test ./...
```

### Run tests for a specific package

```sh
go test ./internal/capture/...
go test ./internal/agent/...
go test ./internal/cli/...
go test ./internal/config/...
```

### Run a specific test by name

```sh
go test ./internal/capture/... -run TestDisk_Run
```

### Run tests with verbose output

```sh
go test -v ./...
```

### Run tests with a timeout

```sh
go test -timeout 120s ./...
```

### Expected output

All packages should report `ok`:

```
ok  yc-agent/internal/agent
ok  yc-agent/internal/agent/api
ok  yc-agent/internal/agent/common
ok  yc-agent/internal/agent/m3
ok  yc-agent/internal/agent/ondemand
ok  yc-agent/internal/capture
ok  yc-agent/internal/capture/executils
ok  yc-agent/internal/capture/ycattach/posix
ok  yc-agent/internal/cli
ok  yc-agent/internal/config
```

> **Note:** `TestExtendedData_Run_Success` may occasionally time out (3 s) on macOS under heavy load. This is a known environment sensitivity — re-running the test suite resolves it.

---

## Test Packages Overview

| Package | What is tested |
|---|---|
| `internal/agent` | Agent lifecycle and coordination |
| `internal/agent/api` | HTTP API server endpoints |
| `internal/agent/common` | Attendance and shared utilities |
| `internal/agent/m3` | M3 metric collection |
| `internal/agent/ondemand` | On-demand capture, zipping, printing |
| `internal/capture` | All artifact captures: GC, heap, thread dump, disk, netstat, ping, vmstat, top, app logs, extended data, etc. |
| `internal/capture/executils` | Shell execution utilities |
| `internal/capture/ycattach/posix` | POSIX jattach integration |
| `internal/cli` | CLI argument validation |
| `internal/config` | Configuration loading and parsing |
