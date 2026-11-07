package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testKey    = "fake-test@00000000-0000-0000-0000-000000000000"
	testSecret = "s3cr3t-pw"
	runTS      = "2026-11-06T10-15-02"
)

// lineKeys are the keys after time, level and msg, in order, for each kind of line.
var lineKeys = map[string][]string{
	"listening":      {"addr", "out", "tls", "api_key"},
	"reply set":      {"path", "status", "delay", "drop", "auth_status"},
	"fin actions":    {"call", "actions", "repeats"},
	"request":        {"method", "path", "dt", "status", "bytes", "duration", "auth", "file", "reply"},
	"request detail": {"path", "query", "header"},
	"violation":      {"rule", "path", "dt", "detail"},
	"upload cut off": {"path", "dt", "bytes", "err", "file"},
	"store failed":   {"path", "dt", "what", "err"},
	"stopped":        {"requests", "violations"},
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// fake is one run of the command, in-process.
type fake struct {
	t      *testing.T
	stdout *syncBuffer
	stderr *syncBuffer
	out    string
	addr   string
	client *http.Client
	scheme string

	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan int
	code     int
}

// startFake runs the command on a free port with -out in a temporary directory.
// prepare runs first, to set up -out.
func startFake(t *testing.T, prepare func(out string), args ...string) *fake {
	t.Helper()

	f := &fake{t: t, stdout: &syncBuffer{}, stderr: &syncBuffer{}, out: filepath.Join(t.TempDir(), "received"),
		done: make(chan int, 1), scheme: "http"}

	require.NoError(t, os.Mkdir(f.out, 0o700))

	if prepare != nil {
		prepare(f.out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel

	all := append([]string{"-addr", "127.0.0.1:0", "-out", f.out, "-api-key", testKey}, args...)

	go func() { f.done <- run(ctx, all, f.stdout, f.stderr) }()

	require.Eventually(t, func() bool { return strings.Contains(f.stdout.String(), "listening") }, 5*time.Second,
		5*time.Millisecond, "no listening line; stderr: %s", f.stderr.String())

	first := strings.SplitN(f.stdout.String(), "\n", 2)[0]
	_, values := parseLine(t, first)
	f.addr = values["addr"]

	// No keep-alive, so the client never retries a dropped request on a fresh connection.
	f.client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	t.Cleanup(func() { f.stop() })

	return f
}

func (f *fake) stop() int {
	f.stopOnce.Do(func() {
		f.cancel()
		f.code = <-f.done
	})

	return f.code
}

func (f *fake) url(path, query string) string {
	return f.scheme + "://" + f.addr + path + "?" + query
}

// post sends a request the way the agent's helpers do and returns the status, or
// 0 when no reply came.
func (f *fake) post(path, query, body string) int {
	f.t.Helper()

	req, err := http.NewRequest(http.MethodPost, f.url(path, query), strings.NewReader(body))
	require.NoError(f.t, err)

	req.Header.Set("ApiKey", testKey)
	req.Header.Set("Content-Type", "text")

	return f.do(req)
}

func (f *fake) get(path, query string) int {
	f.t.Helper()

	req, err := http.NewRequest(http.MethodGet, f.url(path, query), nil)
	require.NoError(f.t, err)

	req.Header.Set("ApiKey", testKey)

	return f.do(req)
}

func (f *fake) do(req *http.Request) int {
	resp, err := f.client.Do(req)
	if err != nil {
		return 0
	}

	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return resp.StatusCode
}

// cutOff sends part of a declared body and closes the connection.
func (f *fake) cutOff(path, query, sent string) {
	f.t.Helper()

	conn, err := net.Dial("tcp", f.addr)
	require.NoError(f.t, err)

	_, err = fmt.Fprintf(conn, "POST %s?%s HTTP/1.1\r\nHost: fake\r\nApiKey: %s\r\nContent-Type: text\r\nContent-Length: %d\r\n\r\n%s",
		path, query, testKey, len(sent)*10, sent)
	require.NoError(f.t, err)
	require.NoError(f.t, conn.Close())
}

// lines parses stdout, which the test reads after the command stopped.
func (f *fake) lines() []parsedLine {
	f.t.Helper()

	var out []parsedLine

	for _, line := range strings.Split(strings.TrimSpace(f.stdout.String()), "\n") {
		keys, values := parseLine(f.t, line)
		out = append(out, parsedLine{raw: line, keys: keys, values: values})
	}

	return out
}

type parsedLine struct {
	raw    string
	keys   []string
	values map[string]string
}

// parseLine reads one log line, text or JSON, keeping the order of its keys.
func parseLine(t *testing.T, line string) ([]string, map[string]string) {
	t.Helper()

	if strings.HasPrefix(line, "{") {
		return parseJSONLine(t, line)
	}

	var keys []string

	values := map[string]string{}
	rest := line

	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		require.Greater(t, eq, 0, "no key in %q", line)

		key := rest[:eq]
		rest = rest[eq+1:]

		var value string

		if strings.HasPrefix(rest, `"`) {
			quoted, err := strconv.QuotedPrefix(rest)
			require.NoError(t, err, line)

			value, err = strconv.Unquote(quoted)
			require.NoError(t, err, line)

			rest = rest[len(quoted):]
		} else if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			value, rest = rest[:sp], rest[sp:]
		} else {
			value, rest = rest, ""
		}

		rest = strings.TrimPrefix(rest, " ")
		keys = append(keys, key)
		values[key] = value
	}

	return keys, values
}

func parseJSONLine(t *testing.T, line string) ([]string, map[string]string) {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()

	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)

	var keys []string

	values := map[string]string{}

	for dec.More() {
		tok, err := dec.Token()
		require.NoError(t, err)

		key := tok.(string)

		var value any
		require.NoError(t, dec.Decode(&value))

		keys = append(keys, key)
		values[key] = fmt.Sprint(value)
	}

	return keys, values
}

// exercise sends one request of every kind the log reports.
func exercise(f *fake) {
	f.t.Helper()

	onDemand := "de=10.0.0.1&ts=" + runTS + "&timezoneId=VVRD"
	m3 := "de=10.0.0.1&ts=" + runTS + "&timezoneID=VVRD"

	assert.Equal(f.t, http.StatusOK, f.post("/ycrash-receiver", onDemand+"&dt=pgSessions", "rows"))
	f.post("/ycrash-receiver", onDemand+"&dt=pgSessions", "more rows")
	f.post("/m3-receiver", m3+"&dt=gc&pid=4711", "gc log")
	f.post("/yc-receiver-heap", onDemand+"&dt=hd&Content-Encoding=zst", "\x28\xb5\x2f\xfd")
	f.post("/ycrash-receiver", onDemand+"&dt=jfr", "\x00\x01\x02 binary")
	f.post("/ycrash-receiver", "de=10.0.0.1&dt=td", "no ts")
	f.post("/ycrash-receiver", onDemand+"&dt=pgSlowQueries", "ALTER ROLE app PASSWORD '"+testSecret+"'")
	f.post("/ycrash-receiver", onDemand+"&dt=ps&apiKey="+testKey, "ps")
	assert.Equal(f.t, http.StatusOK, f.post("/m3-fin", m3+"&cpuCount=8", ""))
	assert.Equal(f.t, http.StatusInternalServerError, f.post("/yc-fin", onDemand, ""))
	assert.Equal(f.t, 0, f.get("/yc-attendance", "type=startup&de=10.0.0.1&ts="+runTS), "attendance is dropped")
	f.post("/ycrash-receiver", "de=10.0.0.1&ts=blocked&timezoneId=VVRD&dt=td", "a run directory the fake cannot write")
	f.cutOff("/yc-receiver-heap", onDemand+"&dt=hd", "part of a heap dump")

	// The cut-off request is recorded once the fake notices the connection closed.
	require.Eventually(f.t, func() bool { return strings.Contains(f.stdout.String(), "upload cut off") },
		5*time.Second, 5*time.Millisecond)
}

// canBlock reports whether a directory can be made unwritable to its owner:
// not on Windows, and not as root, which CI's container runs as.
var canBlock = runtime.GOOS != "windows" && os.Geteuid() != 0

// blockRun makes the yc-blocked run directory one the fake cannot write into.
func blockRun(t *testing.T) func(out string) {
	return func(out string) {
		if !canBlock {
			return
		}

		blocked := filepath.Join(out, "yc-blocked")
		require.NoError(t, os.Mkdir(blocked, 0o500))
		t.Cleanup(func() { os.Chmod(blocked, 0o700) })
	}
}

func writeSecrets(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secrets")
	require.NoError(t, os.WriteFile(path, []byte(testSecret+"\n"), 0o600))

	return path
}

func TestEveryKindOfLineInBothFormats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			f := startFake(t, blockRun(t),
				"-log-format", format, "-log-level", "debug",
				"-status", "yc-fin=500", "-delay", "m3-fin=1ms", "-drop", "yc-attendance",
				"-fin-actions", "", "-fin-actions", "capture db",
				"-secrets-file", writeSecrets(t))

			exercise(f)
			require.Equal(t, 0, f.stop())

			seen := map[string]int{}

			for _, line := range f.lines() {
				msg := line.values["msg"]
				want, known := lineKeys[msg]
				require.True(t, known, "a kind of line the test does not pin: %s", line.raw)

				assert.Equal(t, append([]string{"time", "level", "msg"}, want...), line.keys, line.raw)
				seen[msg]++
			}

			for msg := range lineKeys {
				if msg == "store failed" && !canBlock {
					continue
				}

				assert.NotZero(t, seen[msg], "no %q line", msg)
			}

			assert.Equal(t, 13, seen["request"], "one request line per request")
			assert.Equal(t, 2, seen["fin actions"])
			assert.Equal(t, 3, seen["reply set"], "yc-fin, m3-fin and yc-attendance, in endpoint order")

			stdout := f.stdout.String()
			assert.NotContains(t, stdout, testKey, "the key is never logged")
			assert.NotContains(t, stdout, testSecret, "nor a secret")

			if format == "json" {
				// The run's requests.txt is text whatever stdout uses.
				text, err := os.ReadFile(filepath.Join(f.out, "yc-"+runTS, "requests.txt"))
				require.NoError(t, err)
				assert.True(t, strings.HasPrefix(string(text), "time="), "requests.txt is in the text format")
			}
		})
	}
}

func TestTheValuesOfTheLines(t *testing.T) {
	f := startFake(t, nil, "-status", "yc-fin=500", "-fin-actions", "", "-fin-actions", "capture db", "-secrets-file", writeSecrets(t))

	f.post("/ycrash-receiver", "de=10.0.0.1&ts="+runTS+"&timezoneId=VVRD&dt=pgSessions", "rows")
	f.post("/m3-fin", "de=10.0.0.1&ts="+runTS+"&timezoneID=VVRD&cpuCount=8", "")
	f.post("/m3-fin", "de=10.0.0.1&ts="+runTS+"&timezoneID=VVRD&cpuCount=8", "")
	f.post("/ycrash-receiver", "de=10.0.0.1&ts="+runTS+"&dt=td", "x")
	require.Equal(t, 0, f.stop())

	lines := f.lines()
	require.Len(t, lines, 10)

	byLine := func(i int) map[string]string { return lines[i].values }

	assert.Equal(t, "127.0.0.1", strings.Split(byLine(0)["addr"], ":")[0])
	assert.Equal(t, f.out, byLine(0)["out"])
	assert.Equal(t, "false", byLine(0)["tls"])
	assert.Equal(t, "<masked>", byLine(0)["api_key"], "a key given with -api-key may be a real one")

	assert.Equal(t, map[string]string{"time": byLine(1)["time"], "level": "INFO", "msg": "reply set", "path": "/yc-fin",
		"status": "500", "delay": "0s", "drop": "false", "auth_status": "401"}, byLine(1))
	assert.Equal(t, "", byLine(2)["actions"])
	assert.Equal(t, "false", byLine(2)["repeats"])
	assert.Equal(t, "capture db", byLine(3)["actions"])
	assert.Equal(t, "true", byLine(3)["repeats"])

	upload := byLine(4)
	assert.Equal(t, "INFO", upload["level"])
	assert.Equal(t, "POST", upload["method"])
	assert.Equal(t, "/ycrash-receiver", upload["path"])
	assert.Equal(t, "pgSessions", upload["dt"])
	assert.Equal(t, "200", upload["status"])
	assert.Equal(t, "4", upload["bytes"])
	assert.Equal(t, "ok", upload["auth"])
	assert.Equal(t, "yc-"+runTS+"/pgSessions.txt", upload["file"])
	assert.Equal(t, "OK", upload["reply"])

	assert.Equal(t, `{"actions":[]}`, byLine(5)["reply"])
	assert.Equal(t, `{"actions":["capture db"]}`, byLine(6)["reply"])
	assert.Empty(t, byLine(6)["file"], "a request with no body writes no file")

	assert.Equal(t, "WARN", byLine(8)["level"])
	assert.Equal(t, "query", byLine(8)["rule"])
	assert.Equal(t, "missing timezoneId", byLine(8)["detail"])

	assert.Equal(t, map[string]string{"time": byLine(9)["time"], "level": "INFO", "msg": "stopped",
		"requests": "4", "violations": "1"}, byLine(9))

	// A script finds an upload by its dt, which a space always ends.
	assert.Contains(t, f.stdout.String(), " dt=pgSessions ")
}

func TestLevels(t *testing.T) {
	for _, tt := range []struct {
		level string
		want  []string
	}{
		{"debug", []string{"request", "request detail", "violation", "upload cut off"}},
		{"info", []string{"request", "violation", "upload cut off"}},
		{"warn", []string{"violation", "upload cut off"}},
		{"error", []string{"upload cut off"}},
	} {
		t.Run(tt.level, func(t *testing.T) {
			f := startFake(t, nil, "-log-level", tt.level)

			f.post("/ycrash-receiver", "de=10.0.0.1&ts="+runTS+"&timezoneId=VVRD&dt=td", "x")
			f.post("/ycrash-receiver", "de=10.0.0.1&dt=td", "no ts")
			f.cutOff("/ycrash-receiver", "de=10.0.0.1&ts="+runTS+"&timezoneId=VVRD&dt=gc", "part")
			require.Eventually(t, func() bool {
				text, _ := os.ReadFile(filepath.Join(f.out, "yc-"+runTS, "requests.txt"))
				return strings.Count(string(text), "\n") == 2
			}, 5*time.Second, 5*time.Millisecond)
			require.Equal(t, 0, f.stop())

			seen := map[string]bool{}
			for _, line := range f.lines() {
				seen[line.values["msg"]] = true
			}

			var got []string
			for msg := range seen {
				if msg != "listening" && msg != "stopped" {
					got = append(got, msg)
				}
			}

			sort.Strings(got)

			want := append([]string(nil), tt.want...)
			sort.Strings(want)

			assert.Equal(t, want, got)
			assert.True(t, seen["listening"] && seen["stopped"], "startup and shutdown lines show at every level")
		})
	}
}

func TestLayoutAndNames(t *testing.T) {
	f := startFake(t, nil)

	exerciseNames(f)
	require.Equal(t, 0, f.stop())

	var files []string

	err := filepath.WalkDir(f.out, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(f.out, path)

		info, err := d.Info()
		if err != nil {
			return err
		}

		if runtime.GOOS != "windows" && rel != "." {
			want := os.FileMode(0o600)
			if d.IsDir() {
				want = 0o700
			}

			assert.Equal(t, want, info.Mode().Perm(), rel)
		}

		if !d.IsDir() {
			files = append(files, filepath.ToSlash(rel))
		}

		return nil
	})
	require.NoError(t, err)

	sort.Strings(files)

	run := "yc-" + runTS
	assert.Equal(t, []string{
		"no-ts/requests.txt",
		"no-ts/td.txt",
		"requests.jsonl",
		run + "/gc.pid-4711.txt",
		run + "/hd.zst",
		run + "/jfr.bin",
		run + "/pgSessions-2.txt",
		run + "/pgSessions.txt",
		run + "/requests.txt",
	}, files)

	text, err := os.ReadFile(filepath.Join(f.out, run, "requests.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(text), " file="+run+"/pgSessions-2.txt ", "requests.txt says which request wrote which file")
	assert.Contains(t, string(text), " path=/yc-fin ", "a request with no body is listed too")

	jsonl, err := os.ReadFile(filepath.Join(f.out, "requests.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, 8, strings.Count(string(jsonl), "\n"), "requests.jsonl holds every request")
}

func exerciseNames(f *fake) {
	onDemand := "de=10.0.0.1&ts=" + runTS + "&timezoneId=VVRD"

	f.post("/ycrash-receiver", onDemand+"&dt=pgSessions", "rows")
	f.post("/ycrash-receiver", onDemand+"&dt=pgSessions", "more rows")
	f.post("/m3-receiver", "de=10.0.0.1&ts="+runTS+"&timezoneID=VVRD&dt=gc&pid=4711", "gc log")
	f.post("/yc-receiver-heap", onDemand+"&dt=hd&Content-Encoding=zst", "\x28\xb5\x2f\xfd")
	f.post("/ycrash-receiver", onDemand+"&dt=jfr", "\x00\x01\x02 binary")
	f.post("/ycrash-receiver", "de=10.0.0.1&dt=td", "no ts")
	f.post("/yc-fin", onDemand, "")
	f.get("/yc-attendance", "type=startup&de=10.0.0.1&ts="+runTS)
}

func TestCustomParams(t *testing.T) {
	f := startFake(t, nil, "-custom-params", "tp=pidstat")

	f.post("/ycrash-receiver", "de=10.0.0.1&ts="+runTS+"&timezoneId=VVRD&tp=pidstat", "custom output")
	f.post("/ycrash-receiver", "de=10.0.0.1&ts="+runTS+"&timezoneId=VVRD&tp=vmstat", "not a configured command")
	require.Equal(t, 0, f.stop())

	stdout := f.stdout.String()
	assert.Equal(t, 1, strings.Count(stdout, " msg=violation "), "only the upload of no configured command lacks its dt")
	assert.Contains(t, stdout, "stopped requests=2 violations=1")
}

func TestNoBodies(t *testing.T) {
	f := startFake(t, nil, "-no-bodies")

	f.post("/ycrash-receiver", "de=10.0.0.1&ts="+runTS+"&timezoneId=VVRD&dt=pgSessions", "rows")
	require.Equal(t, 0, f.stop())

	entries, err := os.ReadDir(filepath.Join(f.out, "yc-"+runTS))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "requests.txt", entries[0].Name())
}

func TestTLS(t *testing.T) {
	f := startFake(t, nil, "-tls", "-tls-host", "yc-fake.test")

	ca, err := os.ReadFile(filepath.Join(f.out, "ca.pem"))
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))

	f.scheme = "https"
	f.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}

	assert.Equal(t, http.StatusOK, f.post("/ycrash-receiver", "de=10.0.0.1&ts="+runTS+"&timezoneId=VVRD&dt=td", "x"))

	conn, err := tls.Dial("tcp", f.addr, &tls.Config{RootCAs: pool, ServerName: "yc-fake.test", MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"}})
	require.NoError(t, err, "the certificate covers -tls-host")
	assert.Equal(t, "http/1.1", conn.ConnectionState().NegotiatedProtocol, "no HTTP/2, so a reply can be dropped")
	conn.Close()

	require.Equal(t, 0, f.stop())
	assert.Contains(t, strings.SplitN(f.stdout.String(), "\n", 2)[0], " tls=true")
}

func TestFlagErrors(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(empty, []byte("\n\n"), 0o600))

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"-status", "yc-fin"}, `"yc-fin": want endpoint=value`},
		{[]string{"-status", "report=500"}, `"report" is not an endpoint`},
		{[]string{"-status", "yc-fin=fine"}, `"fine" is not a final HTTP status`},
		{[]string{"-status", "yc-fin=101"}, `"101" is not a final HTTP status`},
		{[]string{"-delay", "yc-fin=soon"}, `"soon" is not a duration`},
		{[]string{"-drop", "yc-fin,report"}, `"report" is not an endpoint`},
		{[]string{"-log-format", "xml"}, `-log-format "xml": want text or json`},
		{[]string{"-log-level", "trace"}, `-log-level "trace": want debug, info, warn or error`},
		{[]string{"-secrets-file", empty}, "holds no secrets"},
		{[]string{"-secrets-file", empty + ".missing"}, "-secrets-file: "},
		{[]string{"stray"}, `unexpected argument "stray"`},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stderr syncBuffer

			code := run(context.Background(), append([]string{"-out", t.TempDir()}, tt.args...), io.Discard, &stderr)
			assert.Equal(t, 2, code)
			assert.Contains(t, stderr.String(), tt.want)
		})
	}
}

// A second fake started by mistake on the same port and -out leaves the running
// one's CA alone.
func TestAPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer ln.Close()

	out := t.TempDir()
	caPath := filepath.Join(out, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte("the running fake's CA"), 0o600))

	var stderr syncBuffer

	code := run(context.Background(), []string{"-addr", ln.Addr().String(), "-out", out, "-tls"}, io.Discard, &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), ln.Addr().String())

	ca, err := os.ReadFile(caPath)
	require.NoError(t, err)
	assert.Equal(t, "the running fake's CA", string(ca))
}

// The default key is a public fixture, so the listening line shows it for the
// agent's -k.
func TestTheListeningLineShowsTheDefaultKey(t *testing.T) {
	cfg, err := parseArgs([]string{"-out", t.TempDir()}, io.Discard)
	require.NoError(t, err)

	var out bytes.Buffer

	logStartup(newLogger(&out, "text", slog.LevelInfo), cfg, "127.0.0.1:18099")
	assert.Contains(t, out.String(), " api_key="+fakeserver.DefaultAPIKey+"\n")
}

// The agent reads caCertPath again after changing into yc-<ts>/, so the CA's path
// the listening line names must not be relative.
func TestOutIsMadeAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, err := parseArgs([]string{"-out", "received"}, io.Discard)
	require.NoError(t, err)

	cwd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "received"), cfg.out)
}

func TestHelp(t *testing.T) {
	var stderr syncBuffer

	assert.Equal(t, 0, run(context.Background(), []string{"-h"}, io.Discard, &stderr))

	help := stderr.String()
	assert.Contains(t, help, "Usage: yc-fake-server [flags]")
	assert.Contains(t, help, "ycrash-receiver, yc-receiver-heap, yc-fin, m3-receiver, m3-fin, yc-attendance")

	for _, flag := range []string{"addr", "api-key", "fin-actions", "custom-params", "secrets-file", "status", "delay", "drop",
		"tls", "tls-host", "out", "no-bodies", "log-format", "log-level"} {
		assert.Regexp(t, `(?m)^  -`+flag+`( |$)`, help, flag)
	}
}
