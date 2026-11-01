# How to verify the JFR start-refusal fix by hand

Fix under test: `a82ba61 fix(jfr): confirm recording startup before continuing or cleaning up`
(on `main`).

## The bug

On a JVM that is out of heap, `jcmd <pid> JFR.start ...` prints `Java heap space` and
still exits **0**. In 3.12 and earlier, yc treated that as a success:

1. It logged `JFR recording ycJFR-... started, running for 1m0s`.
2. It slept for the whole recording window.
3. On JDK 8 the JVM had created the recording but never started it, so `JFR.check`
   kept listing it as `(new)`. A recording that never started never finishes, so yc
   polled for 20 s and then gave up with the misleading
   `could not confirm within 20s that the JVM had finished writing JFR recording ...`.
4. It left an empty stub file in `/tmp/my.jfr.<pid>.<nanos>`.

Note: in the customer's `log.txt`, the `: 1` after `JFR.start ... duration=60s` is the
first character of the `1784893:` PID header, cut off at the terminal width. It is not
an exit code.

The test below uses an OpenJDK 8 container with the customer's JDK version (8u504-b01).

## Prerequisites

- Docker. `make build-all` builds inside Alpine, and the test JVM runs in
  `eclipse-temurin:8-jdk`.
- `FillHeapAndHold.java` in this folder. It fills the heap almost completely and then
  frees a 2 MB reserve, so its own thread survives. Next it creates the file named in its
  first argument, prints `FillHeapAndHold: heap is full, ready (<file>)` to stdout, and
  sleeps forever. An optional second argument sets the reserve in bytes. The table in the
  file's header comment shows how the reserve size changes the `JFR.start` failure. With
  2 MB, Temurin 8u504 fails exactly as the customer's JVM did.

## 1. Build the old and fixed binaries

Run these from the repo root:

```sh
make build-all && cp bin/linux/amd64/yc /tmp/yc-fixed

git worktree add /tmp/yc-prefix a82ba61~1
(cd /tmp/yc-prefix && make build-all && cp bin/linux/amd64/yc /tmp/yc-old)
git worktree remove /tmp/yc-prefix
```

The binaries are statically linked, so they run in the Temurin container.

`make build-all` builds arm64 after amd64. On an x86-64 host without QEMU binfmt, the arm64
leg fails with `exec format error` and `make` exits 2. The amd64 binary is already built at
that point, and it is the only one this test needs. Run `cp` on its own after the failed
`make`, because `&&` skips it.

## 2. Start the container

```sh
docker run -it --rm \
  -v /tmp/yc-old:/yc-old -v /tmp/yc-fixed:/yc-fixed \
  -v "$PWD/jfr-start-heap-full":/repro -w /repro \
  eclipse-temurin:8-jdk bash
```

Inside the container, install `zstd` so you can inspect yc's archives, compile, and define a
helper that starts a fresh heap-full JVM and sets `$PID`:

```sh
apt-get -qq update && apt-get -qq install -y zstd
javac FillHeapAndHold.java

full_jvm() {
  rm -f ready
  java -Xmx32m -XX:+UseSerialGC -cp . FillHeapAndHold /repro/ready &
  PID=$!
  while [ ! -e ready ]; do sleep 1; done
  echo "heap-full JVM: $PID"
}
```

**Each heap-full JVM can be used for one `JFR.start` only.** The first `JFR.start` fails
partway through and leaves JFR's internal classes broken. After that, every `JFR.start`
answers `Could not initialize class jdk.jfr.internal.Logger` and registers no recording,
which is a different code path from the one under test. That is why steps 3 and 4, and
the optional probe below, each start their own JVM and kill it when they are done.

### Optional: confirm the JVM refuses JFR.start

```sh
full_jvm
jcmd $PID JFR.start name=probe filename=/tmp/probe.jfr duration=10s; echo exit=$?
jcmd $PID JFR.check
kill -9 $PID
```

Expected:

```
<pid>:
Java heap space
exit=0
<pid>:
Recording 1: name=probe duration=10s (new)
```

This matches the customer's log. Kill this JVM afterwards. Don't reuse it for steps 3 or 4.

## 3. Old binary: reproduce the bug

```sh
full_jvm
rm -f /tmp/my.jfr.* yc-*.zst
/yc-old -p $PID -onlyCapture -j /opt/java/openjdk -jfrCaptureDuration 10s
ls -la /tmp/my.jfr.*
kill -9 $PID
```

Expected:

- `jcmd JFR.start ...: <pid>:` followed by `Java heap space`.
- Then the misleading line `JFR recording ycJFR-... started, running for 10s; ...`.
- After 10 s, about ten repeats of `jcmd JFR.check ... (new)` and
  `has not finished writing yet; checking again in 2s`.
- Then `WARNING: leaving /tmp/my.jfr.... alone` and the JFR result
  `capture failed: could not confirm within 20s that the JVM had finished writing JFR recording ...`.
- The JFR step takes about 30 s, and `ls` shows a leftover 0-byte `/tmp/my.jfr.<pid>.<nanos>`.

## 4. Fixed binary: verify the fix

```sh
full_jvm
rm -f /tmp/my.jfr.* yc-*.zst
/yc-fixed -p $PID -onlyCapture -j /opt/java/openjdk -jfrCaptureDuration 10s
ls -la /tmp/my.jfr.*
zstd -dc yc-*.zst | tar -t | grep my.jfr || echo "no my.jfr in archive"
kill -9 $PID
```

Expected:

- `jcmd JFR.start ...: <pid>:` followed by `Java heap space`.
- **No** `JFR recording ... started` line.
- A single `jcmd JFR.check` straight away, which lists the recording as `(new)`.
- The JFR result fails immediately, giving the real reason:

  ```
  failed to start JFR recording: the JVM answered JFR.start with "Java heap space"; JFR.check lists recording ycJFR-... as new, never started
  ```

- `ls` finds no `/tmp/my.jfr.*`, because the empty stub was removed.
- The other artifacts are captured as usual. `yc-<timestamp>.zst`, a zstd-compressed tar,
  is written with no `my.jfr` in it.

## 5. Check that normal captures still work

Still inside the container, start a JVM that has plenty of heap, then capture from it.
`FillHeapAndHold` is no good here, because it fills whatever heap it is given.

```sh
cat > Idle.java <<'EOF'
public class Idle { public static void main(String[] a) throws Exception { Thread.sleep(Long.MAX_VALUE); } }
EOF
javac Idle.java
java -Xmx512m -cp . Idle &
IDLE=$!
sleep 2
rm -f yc-*.zst
/yc-fixed -p $IDLE -onlyCapture -j /opt/java/openjdk -jfrCaptureDuration 10s
zstd -dc yc-*.zst | tar -tv | grep my.jfr
kill $IDLE
```

Expected:

- `jcmd JFR.start ...: <pid>: Started recording 1. ...`
- `JFR recording ycJFR-... started, running for 10s; ...`
- After about 10 s, a `JFR.check` in which the recording is closed or no longer listed,
  then `staged JFR recording as my.jfr (N bytes)` with N > 0.
- `yc-<timestamp>.zst` contains `yc-<timestamp>/my.jfr`.

To check that the recording is valid, extract it with `zstd -dc yc-*.zst | tar -x` and run
`jfr summary yc-<timestamp>/my.jfr`. The JDK 8 image has no `jfr` tool, so use the JDK 21
one on the host. The code itself does not check the file's validity yet.

## Clean up

```sh
exit                                   # leave the container (--rm deletes it)
rm -f /tmp/yc-old /tmp/yc-fixed
rm -f jfr-start-heap-full/*.class jfr-start-heap-full/ready jfr-start-heap-full/Idle.java \
  jfr-start-heap-full/yc-*.zst
```

The container writes these files as root. You can still delete them without `sudo`,
because the folder itself is yours. The folder's `.gitignore` and the repo's `yc-*` rule
keep these files out of commits anyway.

## Already verified

On JDK 8, in `eclipse-temurin:8-jdk` (1.8.0_504-b01), with a fresh heap-full JVM (2 MB
reserve) for each run:

| Run | Outcome |
|---|---|
| Pre-fix binary | Logged "started", waited 10 s, polled `JFR.check` (`(new)` every time) for 20 s, then failed with the misleading timeout message and left a 0-byte stub. Matches the customer's `log.txt`. |
| Fixed binary | Failed straight away with `the JVM answered JFR.start with "Java heap space"; JFR.check lists recording ... as new, never started`. No stub left, and no `my.jfr` in the archive. |
| Fixed binary, normal JVM (step 5) | Staged a 184 KB `my.jfr`, and `jfr summary` reads it as a valid 10 s recording. |
| Fixed binary, 64 KB reserve | `JFR.start` printed a lambda-form dump and registered nothing. Failed straight away with `JFR.check does not list recording ...`. |

On JDK 21, on the host:

| Run | Outcome |
|---|---|
| Fixed code vs heap-full JDK 21 | Failed in 173 ms: `the JVM answered JFR.start with "Could not initialize class jdk.jfr.internal.Logger"; could not check whether recording ... started` (on 21 the heap-full JVM also breaks `JFR.check`) |
| Pre-fix code vs the same JVM | Logged "started", then failed after 31 s with the misleading timeout message |
| `go test -race` on the JFR tests, `golangci-lint` | Pass, 0 issues |
