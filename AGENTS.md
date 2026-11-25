# AGENTS.md

## What this is

yc-360 script: a single Go binary (`yc`, module name `yc-agent`) that captures diagnostic artifacts from a target Java, .NET or Node.js process and its host. It collects GC logs, thread dumps, heap dumps or heap substitutes, JFR, top, ps, netstat, vmstat, dmesg, disk, ping, kernel params, app/access logs and extended data. It then uploads them to a yCrash server, or with `-onlyCapture` packs them locally into `yc-<timestamp>.zst`, a zstd-compressed tar file.

## Commands

The Go version is pinned in `go.mod`. The Makefile, Dockerfiles and CI all read it from there.

```sh
# Build for the host (cgo is required)
cd cmd/yc && go build -o yc -ldflags='-s -w' -buildvcs=false && ./yc -version

# Static Linux release binaries -> bin/linux/{amd64,arm64}/yc (Docker buildx, Dockerfile.base.alpine)
make build-all

# Tests
go test ./...
go test ./internal/capture/ -run TestDisk_Run -v      # single test
go test -race -timeout 30m -buildvcs=false ./...      # what CI runs

# Lint (golangci-lint v2, config in .golangci.yml; CI uses `latest`)
golangci-lint run ./...
```

CI (`.github/workflows/ci.yml`) is the source of truth. It runs golangci-lint, a gitleaks scan of the working tree, an Alpine build, and `go test -race ./...` inside `golang:<go.mod version>-alpine` with `gcc musl-dev ncurses-dev ncurses-static git openjdk11`. `docs/build/run_ci_locally.md` describes jobs and package exclusions that CI no longer has.

### Linux-only code on macOS

Many tests and files are Linux-only (`/proc` reading, `_linux.go` files). Run them in Docker. Mount the host module cache, and also mount the build cache and temp dir, because a `-race` build can fill the Docker VM disk:

```sh
docker run --rm -v "$PWD":/src -v "$(go env GOMODCACHE)":/go/pkg/mod \
  -v <host-cache>:/root/.cache/go-build -v <host-tmp>:/gotmp -e GOTMPDIR=/gotmp \
  -e GOFLAGS=-buildvcs=false -w /src golang:<go.mod version> \
  go test -race ./internal/capture/ -run <Test> -v
```

On Linux, only `internal/cli` and `cmd/yc` import `internal/capture/procps/linux`. That package uses cgo to compile vendored procps `top`/`vmstat` C sources and links `libncursesw.a` statically. Building those two packages therefore needs the Alpine image with `ncurses-static`; every other package builds in the plain `golang` image.

### Secrets in fixtures

`.gitleaks.toml` flags anything shaped like a yCrash API key (`name@<uuid>`). Test fixtures must use the placeholder `@00000000-0000-0000-0000-000000000000`.

## Architecture

### Startup and the binary's own sub-modes (`internal/cli`)

`cmd/yc/main.go` calls `cli.Run()`, which runs these steps in order:

1. **Raw modes** `-vmstatMode` / `-topMode`. These are checked on `os.Args[1]` *before* flag parsing, so their remaining args pass straight through to the bundled procps C `top`/`vmstat`. They work on Linux only; other platforms print "Not implemented".
2. `config.ParseFlags`, then logger init.
3. **jattach modes** `-gcCaptureMode`, `-tdCaptureMode`, `-hdCaptureMode`, `-jCmdCaptureMode <cmd>`. Each one attaches to the JVM through `internal/capture/ycattach` (vendored jattach C via cgo) and then exits.
4. Validation, then `agent.Run` under a SIGTERM/SIGINT handler.

These sub-modes are mostly an internal interface. The capture code **re-executes its own binary** (`executils.Executable()`) to use them: `yc -topMode ...` for top, and `yc -p <pid> -gcCaptureMode` (and the other jattach modes) for GC, thread dump, heap dump and jcmd. These calls are wrapped in `SudoHooker`/`EnvHooker` so that they run as the target process's user. If that fails, the code copies the binary to a temp path (`Copy2TempPath`) and retries. Changing these flags or their output changes the main capture flow.

### Run modes (`internal/agent`)

`agent.Run` chooses a mode from the config:
- **OnDemand**: selected when `-p` is set. `-p` takes a PID or a process token, and a token is resolved to PIDs. `-onlyCapture` uses the same code path but skips the upload. The flow is `ondemand.FullCapture` (`internal/agent/ondemand/ondemand.go`).
- **M3**: selected by `-m3`. A periodic capture loop in `internal/agent/m3`. It asks the server which PIDs to fully capture next (`ParseM3FinResponse`).
- **API**: selected by `-port`. An HTTP server whose `/action` endpoint (`internal/agent/api`) triggers `ondemand.ProcessPids`.

OnDemand and M3 conflict with each other. M3 and API block forever.

### The capture pipeline

- `FullCapture` creates `yc-<timestamp>/` and **`os.Chdir`s into it**. Every capture task writes its artifact by a relative file name in the current working directory. When the capture finishes, the deferred code changes back to the original directory and in `-onlyCapture` mode packs the folder into `yc-<timestamp>.zst`. M3 skips the directory and chdir steps.
- Each artifact type is a struct in `internal/capture` that embeds `capture.Capture` and implements `Task` (`Run() (Result, error)`). The orchestrator starts each one as `goCapture(endpoint, capture.WrapRun(task), waitFor...)`, which runs it in a goroutine and returns a result channel. Any `waitFor` tasks must finish before this one starts, for example dmesg waits for vmstat.
- Each task uploads its own output inside `Run()` through `PostData`/`PostCustomData*`, sent to the yc server endpoint with a `dt=<type>` query param and the `ApiKey` header. In `-onlyCapture` mode these functions return early without sending anything.
- `config.GetAppRuntime(pid)` selects the runtime-specific captures. It uses `-appRuntime` if set; otherwise it auto-detects .NET (Windows module inspection), then Node.js, and falls back to Java. Host-level captures run for every runtime.
  - **Java**: the GC log path is taken from flags or from the target's cmdline. Thread dumps, heap dumps and jcmd go through the self re-exec described above. JFR is optional (`-jfrEnabled`; skipped with `-minimalTouch`).
  - **.NET**: Windows only. Captures are delegated to an external `yc-dot-net-x86.exe`/`yc-dot-net-x64.exe` that sits next to `yc` (`-dotnetToolPath`).
  - **Node.js**: talks to the yCrash hook (`yc360-node-hook.js`, which is *not* in this repo) already loaded in the target. Discovery uses a registration file in the runtime dir. Requests go as line-delimited JSON over a Unix socket on POSIX or a named pipe on Windows (`nodejs_ipc*.go`, `nodejs_protocol.go`, `nodejs_rpc.go`). Without the hook, it falls back to `-nodejsCaptureMode signal`.

### Config (`internal/config`)

`Options` is the single source of truth for flags and YAML. For each field, the `yaml` tag is both the YAML key and the CLI flag name, a field with only an `arg` tag is CLI-only, and the `usage` tag is the help text. Flags are registered by reflection. Precedence: CLI flags are applied, then the YAML file from `-c`, then the CLI flags again, so the CLI wins. Defaults live in `defaultConfig()`. Everything reads the global `config.GlobalConfig`.

### Platform-specific code

Platform-specific code uses file suffixes or build tags: `_linux`, `_darwin`, `_windows`, `_aix`, `_solaris`, and `_others` for the fallback. The external shell command for each artifact is defined per OS in `internal/capture/executils/const_<os>.go`. A behavior change usually has to be checked in several of these files. Windows no longer uses `wmic` (it was removed in Windows 11 24H2). The recorded wmic tables in `testdata/wmic/` are byte-exact characterization fixtures, so `.gitattributes` marks them `-text`. Re-record them on Windows with `YC_RECORD_WMIC_FIXTURES=1 go test ./internal/capture/ -run TestRecordWMICFixtures`.

### Version

`SCRIPT_VERSION` in `internal/capture/executils/const.go`. Releases bump it in commits named "version updated to X.Y".

## Testing gotchas

- `internal/capture/heap_test.go`'s `init()` does `os.Chdir("testdata")` for the whole `capture` test binary. Many other tests also chdir into temp dirs. As a result, capture code that writes to the cwd during tests writes into `internal/capture/testdata/`. Stray untracked `ed-*` files there come from the extended-data tests: ExtendedData copies collected files into the cwd with an `ed-` prefix. Don't commit them, and don't use `t.Parallel()` in tests that depend on the cwd.
- Java-dependent tests call `skipIfNoJava` and skip unless `JAVA_HOME` is set and `java` is on PATH. CI installs openjdk11.
- Tests stub external commands and network calls through func-typed struct fields or package-level function variables (e.g. `goCapture`, `getOutboundIP` in `ondemand`). That is why `.golangci.yml` turns off `unparam` for `_test.go` files.
- Lint is lenient by design (`errcheck` off, `gocyclo` at 120, revive's `exported`/`var-naming` off). Don't "fix" those across the codebase.
