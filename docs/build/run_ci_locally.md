# Running CI Locally

This guide explains how to run the same checks that `.github/workflows/ci.yml` performs, both via [`act`](https://github.com/nektos/act) (runs the actual workflow file in Docker) and manually (runs each step directly on your machine).

---

## Known Issues in ci.yml

### 1. `gofmt -l .` scans the `vendor/` directory

**Location:** `golangci-lint` job, step _"Check for formatting issues"_

```yaml
if [ -n "$(gofmt -l .)" ]; then
```

`gofmt -l .` recurses into `vendor/`, flagging formatting differences in third-party code that is not the project's responsibility. This can cause false CI failures after a `go mod vendor`.

**Fix:** Scope the check to project files only:

```yaml
if [ -n "$(gofmt -l $(go list -f '{{.Dir}}' ./...))" ]; then
```

---

### 2. `golangci-lint-action` pins `version: latest`

**Location:** `golangci-lint` job, step _"Run golangci-lint"_

```yaml
uses: golangci/golangci-lint-action@v9
with:
  version: latest
```

Using `latest` means any new golangci-lint release can introduce new default linters or stricter rules that break CI without any code changes. Pin to a specific release instead:

```yaml
with:
  version: v2.5.0
```

---

### 3. `./yc --version || true` silences binary execution errors

**Location:** `build` job, step _"Build binary"_

```yaml
./yc --version || true
```

The `|| true` suppresses a non-zero exit code, so if the binary crashes or `--version` is unsupported, the build job still passes. This makes the smoke-test meaningless.

**Fix:** Either remove the line (just verifying the binary exists is sufficient) or check the binary exists explicitly:

```yaml
ls -lh yc
```

---

### 4. Redundant `gofmt` step

**Location:** `golangci-lint` job, step _"Check for formatting issues"_

The project's `.golangci.yml` uses `default: standard` which already includes the `gofmt` linter. The separate manual `gofmt -l .` step duplicates that check. It can be removed once issue #1 above is fixed in `.golangci.yml` instead.

---

## Option A — Run the Full Workflow with `act`

[`act`](https://github.com/nektos/act) runs GitHub Actions workflows locally inside Docker containers.

### Prerequisites

- Docker Desktop running
- `act` installed (`brew install act` on macOS)

Verify:

```sh
act --version   # should print act version 0.2.x or later
docker info     # should not error
```

### Run all CI jobs

```sh
act push
```

### Run a single job by job ID

```sh
act push --job golangci-lint
act push --job go-vet
act push --job staticcheck
act push --job build
act push --job test
```

### Use a medium-sized runner image (faster than full ubuntu image)

```sh
act push -P ubuntu-latest=catthehacker/ubuntu:act-latest
```

### Dry-run (list jobs without executing)

```sh
act push --dryrun
```

> **Note:** The `build` job in ci.yml runs inside a `golang:1.25.8-alpine` container. `act` supports this via the `container:` key in the workflow — no extra configuration needed.

---

## Option B — Run Each CI Job Manually

Use this when you want fast feedback without spinning up Docker containers.

### 1. golangci-lint

Requires `golangci-lint` installed. Install it:

```sh
brew install golangci-lint          # macOS
# or
go install github.com/golangci/golangci-lint/cmd/golangci-lint@v2.5.0
```

Run lint:

```sh
golangci-lint run ./...
```

Run the formatting check (vendor-safe):

```sh
unformatted=$(gofmt -l $(go list -f '{{.Dir}}' ./...))
if [ -n "$unformatted" ]; then
  echo "Formatting issues found:"
  gofmt -d $unformatted
  exit 1
fi
echo "All files formatted correctly."
```

### 2. go vet

```sh
go vet ./...
```

### 3. staticcheck

Install:

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
```

Run:

```sh
staticcheck ./...
```

### 4. Build (Alpine — matches CI container)

The CI build job runs inside `golang:1.25.8-alpine`. To replicate exactly:

```sh
docker run --rm \
  -v $(pwd):/app \
  -w /app \
  golang:1.25.8-alpine \
  sh -c "apk add --no-cache gcc musl-dev ncurses-dev ncurses-static git && cd cmd/yc && go build -buildvcs=false -ldflags='-s -w' -o yc && ls -lh yc"
```

Or use the Makefile (see [build_and_test.md](./build_and_test.md)):

```sh
make alpine base build
```

### 5. Tests (with race detector and coverage)

This matches the `test` job exactly. CGO-dependent packages are excluded because they require `musl-dev` and `ncurses` which are not present on a standard Ubuntu/macOS host.

```sh
go test -v -race \
  -coverprofile=coverage.out \
  -covermode=atomic \
  $(go list ./... | grep -v 'internal/cli' | grep -v 'internal/capture/procps/linux')
```

View the coverage summary:

```sh
go tool cover -func=coverage.out | tail -20
go tool cover -func=coverage.out | grep total
```

Open an HTML coverage report in the browser:

```sh
go tool cover -html=coverage.out
```

> **Why are `internal/cli` and `internal/capture/procps/linux` excluded?**
> Both packages use CGO (`import "C"`) and depend on native libraries (`ncurses`, `musl`, `procps`) that are not available on a standard macOS or Ubuntu runner. They build correctly inside the Alpine Docker container used in the `build` job.

---

## Quick Pre-commit Checklist

Run these before every commit to catch issues early:

```sh
# 1. Format
gofmt -w $(go list -f '{{.Dir}}' ./...)

# 2. Vet
go vet ./...

# 3. Lint
golangci-lint run ./...

# 4. Test
go test -race $(go list ./... | grep -v 'internal/cli' | grep -v 'internal/capture/procps/linux')
```
