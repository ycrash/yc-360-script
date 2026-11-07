package fakeserver_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// run is the directory URLFor's requests are filed under.
const run = "yc-2006-01-02T15-04-05"

func files(t *testing.T, dir string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		out = append(out, filepath.ToSlash(rel))

		return err
	})
	require.NoError(t, err)

	sort.Strings(out)

	return out
}

func TestFilesAreNamedFromTheRequest(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})
	receiver := srv.URLFor(fakeserver.Receiver)

	post(t, srv, receiver+"&dt=pgSessions", "first")
	post(t, srv, receiver+"&dt=pgSessions", "second")
	post(t, srv, srv.URLFor(fakeserver.M3Receiver)+"&dt=gc&pid=4711", "gc log")
	post(t, srv, srv.URLFor(fakeserver.HeapReceiver)+"&dt=hd&Content-Encoding=zst", "\x28\xb5\x2f\xfd compressed")
	post(t, srv, receiver+"&dt=jfr", "\x00\x01\x02\x03 binary")
	post(t, srv, receiver+"&dt=td&pid=../../etc", "a pid that is not a number")
	post(t, srv, srv.URLFor(fakeserver.Fin), "")
	post(t, srv, srv.URL()+string(fakeserver.Receiver)+"?dt=td", "no ts")

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
		run + "/td.pid-_.._etc.txt",
	}, files(t, srv.Dir()))

	requests := srv.Requests()
	assert.Equal(t, run+"/pgSessions.txt", requests[0].File)
	assert.Equal(t, run+"/pgSessions-2.txt", requests[1].File)
	assert.Empty(t, requests[6].File, "a request with no body writes no file")

	body, err := requests[1].Body()
	require.NoError(t, err)
	assert.Equal(t, "second", string(body))

	body, err = requests[6].Body()
	require.NoError(t, err)
	assert.Empty(t, body, "and reads back empty")

	text, err := os.ReadFile(filepath.Join(srv.Dir(), run, "requests.txt"))
	require.NoError(t, err)
	assert.Equal(t, 7, strings.Count(string(text), " msg=request "), "every request of the run is listed, bodies or not")
	assert.Contains(t, string(text), " file="+run+"/pgSessions-2.txt ")
}

func TestAnUploadWithNoDTIsFiledAsNoDT(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{CustomParams: []string{"tp=pidstat"}})

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&tp=pidstat", "custom output")

	assert.Equal(t, run+"/no-dt.txt", srv.Requests()[0].File)
}

func TestABodyIsNeverFiledAsRequestsTxt(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=requests", "the body")
	post(t, srv, srv.URLFor(fakeserver.Fin), "")

	r := srv.Requests()[0]
	assert.Equal(t, run+"/requests-2.txt", r.File)

	body, err := r.Body()
	require.NoError(t, err)
	assert.Equal(t, "the body", string(body))

	text, err := os.ReadFile(filepath.Join(srv.Dir(), run, "requests.txt"))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(text), " msg=request "))
	assert.NotContains(t, string(text), "the body")
}

func TestContentEncodingIsMaskedInTheFileName(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Secrets: []string{"hunter2"}})

	post(t, srv, srv.URLFor(fakeserver.HeapReceiver)+"&dt=hd&Content-Encoding=hunter2", "x")
	post(t, srv, srv.URLFor(fakeserver.HeapReceiver)+"&dt=hd&Content-Encoding="+url.QueryEscape(srv.APIKey()), "x")

	requests := srv.Requests()
	assert.Equal(t, run+"/hd.secret1", requests[0].File)
	assert.Equal(t, run+"/hd.apikey", requests[1].File)

	jsonl, err := os.ReadFile(filepath.Join(srv.Dir(), "requests.jsonl"))
	require.NoError(t, err)
	assert.NotContains(t, string(jsonl), "hunter2")
	assert.NotContains(t, string(jsonl), "fakeserver0", "nor the key's letters and digits")
}

func TestAnExistingFileIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, run), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, run, "td.txt"), []byte("an earlier run"), 0o600))

	srv := fakeserver.Start(t, fakeserver.Options{Dir: dir})
	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", "this run")

	assert.Equal(t, run+"/td-2.txt", srv.Requests()[0].File)

	earlier, err := os.ReadFile(filepath.Join(dir, run, "td.txt"))
	require.NoError(t, err)
	assert.Equal(t, "an earlier run", string(earlier))
}

// On a file system that ignores case, Requests.txt would be the run's requests.txt.
func TestABodyIsNeverFiledAsRequestsTxtInAnyCase(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=Requests", "the body")
	post(t, srv, srv.URLFor(fakeserver.Fin), "")

	assert.Equal(t, run+"/Requests-2.txt", srv.Requests()[0].File)

	text, err := os.ReadFile(filepath.Join(srv.Dir(), run, "requests.txt"))
	require.NoError(t, err)
	assert.NotContains(t, string(text), "the body")
}

// Started again on the same Dir, a fake numbers on from the requests already in
// requests.jsonl, so every seq there stays unique.
func TestAFakeStartedAgainNumbersOn(t *testing.T) {
	dir := t.TempDir()

	first := fakeserver.Start(t, fakeserver.Options{Dir: dir})
	post(t, first, first.URLFor(fakeserver.Fin), "")
	post(t, first, first.URLFor(fakeserver.Fin), "")

	second := fakeserver.Start(t, fakeserver.Options{Dir: dir})
	post(t, second, second.URLFor(fakeserver.Fin), "")
	assert.Equal(t, 3, second.Requests()[0].Seq)

	// A line cut short when a fake stopped is skipped.
	f, err := os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"seq":9,"time":`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	third := fakeserver.Start(t, fakeserver.Options{Dir: dir})
	post(t, third, third.URLFor(fakeserver.Fin), "")
	assert.Equal(t, 4, third.Requests()[0].Seq)
}

func TestAnEmptyBodyReadsBackEmpty(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", "")

	r := srv.Requests()[0]
	assert.Empty(t, r.File, "no file for an empty body")

	body, err := r.Body()
	require.NoError(t, err)
	assert.Empty(t, body)
}

func TestSizeAndHashOfEveryBody(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{NoBodies: true})

	body := strings.Repeat("row\n", 10_000)
	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", body)

	sum := sha256.Sum256([]byte(body))
	r := srv.Requests()[0]
	assert.Equal(t, int64(len(body)), r.Bytes)
	assert.Equal(t, hex.EncodeToString(sum[:]), r.SHA256)
	assert.Empty(t, r.File, "NoBodies keeps only the size and the hash")

	assert.Equal(t, []string{"requests.jsonl", run + "/requests.txt"}, files(t, srv.Dir()))
}

func TestFilesAreReadableByTheOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}

	srv := fakeserver.Start(t, fakeserver.Options{TLS: true})
	send(t, agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Receiver)+"&dt=pgSessions", "query text"),
		trusting(t, srv.CACertPath()))

	for _, name := range []string{run, run + "/pgSessions.txt", run + "/requests.txt", "requests.jsonl", "ca.pem"} {
		info, err := os.Stat(filepath.Join(srv.Dir(), name))
		require.NoError(t, err)

		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}

		assert.Equal(t, want, info.Mode().Perm(), name)
	}
}

func TestRequestsJSONLHoldsEveryRecord(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
	post(t, srv, srv.URL()+string(fakeserver.Receiver)+"?dt=gc", "y")

	f, err := os.Open(filepath.Join(srv.Dir(), "requests.jsonl"))
	require.NoError(t, err)

	defer f.Close()

	var records []fakeserver.Request

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r fakeserver.Request
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &r))
		records = append(records, r)
	}

	require.Len(t, records, 2)
	assert.Equal(t, "td", records[0].DT)
	assert.Equal(t, run+"/td.txt", records[0].File)
	assert.Equal(t, "td", records[0].Query.Get("dt"))
	assert.Equal(t, http.StatusOK, records[0].Status)
	assert.Empty(t, records[0].Violations)
	assert.Equal(t, "missing de, ts, timezoneId", records[1].Violations[0].Detail)
}

// rawUpload sends a request line, headers and part of a body, then closes the
// connection: an agent stopped mid-upload.
func rawUpload(t *testing.T, srv *fakeserver.Server, target string, declared int, sent string) {
	t.Helper()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL(), "http://"))
	require.NoError(t, err)

	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: fake\r\nApiKey: %s\r\nContent-Type: text\r\nContent-Length: %d\r\n\r\n%s",
		target, srv.APIKey(), declared, sent)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestAnUploadCutOffKeepsWhatArrived(t *testing.T) {
	var logged syncBuffer

	srv := fakeserver.Start(t, fakeserver.Options{Log: debugLog(&logged)})

	target := strings.TrimPrefix(srv.URLFor(fakeserver.HeapReceiver), srv.URL()) + "&dt=hd&Content-Encoding=zst"
	rawUpload(t, srv, target, 1000, "only part of the heap dump")

	require.Eventually(t, func() bool { return len(srv.Requests()) == 1 }, 5*time.Second, 10*time.Millisecond)

	r := srv.Requests()[0]
	assert.Equal(t, 0, r.Status)
	assert.Equal(t, "upload cut off: unexpected EOF", r.Err)
	assert.Equal(t, int64(len("only part of the heap dump")), r.Bytes)

	body, err := r.Body()
	require.NoError(t, err)
	assert.Equal(t, "only part of the heap dump", string(body))

	assert.Contains(t, logged.String(), `level=ERROR msg="upload cut off" path=/yc-receiver-heap dt=hd bytes=26 err="unexpected EOF" file=`+run+"/hd.zst\n")
}

// A client that stops sending but still reads must get no reply: net/http would
// otherwise answer 200 for a handler that returns without writing.
func TestACutOffUploadGetsNoReply(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL(), "http://"))
	require.NoError(t, err)

	defer conn.Close()

	target := strings.TrimPrefix(srv.URLFor(fakeserver.Receiver), srv.URL()) + "&dt=td"
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: fake\r\nApiKey: %s\r\nContent-Type: text\r\nContent-Length: 1000\r\n\r\npartial",
		target, srv.APIKey())
	require.NoError(t, err)
	require.NoError(t, conn.(*net.TCPConn).CloseWrite())

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	reply, _ := io.ReadAll(conn)
	assert.Empty(t, string(reply))

	require.Len(t, srv.Requests(), 1)
	assert.Equal(t, 0, srv.Requests()[0].Status)
}

func TestAFileTheFakeCannotWriteIsReportedAndTheReplyStands(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory its owner cannot write to")
	}

	var logged syncBuffer

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, run), 0o500))
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, run), 0o700) })

	srv := fakeserver.Start(t, fakeserver.Options{Dir: dir, Log: debugLog(&logged)})

	status, _ := post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
	assert.Equal(t, http.StatusOK, status, "the agent is not at fault")

	assert.Contains(t, logged.String(), `level=ERROR msg="store failed" path=/ycrash-receiver dt=td what=body err=`)
	assert.Contains(t, logged.String(), `level=ERROR msg="store failed" path=/ycrash-receiver dt=td what=requests.txt err=`)
	assert.Empty(t, srv.Requests()[0].File)
}

func TestParallelUploadsKeepTheirOwnBodies(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	const n = 40

	var wg sync.WaitGroup

	for i := range n {
		wg.Add(1)

		go func() {
			defer wg.Done()

			req := agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Receiver)+"&dt=td", fmt.Sprintf("body %d", i))

			resp, err := http.DefaultClient.Do(req)
			if assert.NoError(t, err) {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	wg.Wait()

	requests := srv.Requests()
	require.Len(t, requests, n)

	seenFiles := map[string]bool{}
	seenBodies := map[string]bool{}

	for _, r := range requests {
		body, err := r.Body()
		require.NoError(t, err)

		seenFiles[r.File] = true
		seenBodies[string(body)] = true
	}

	assert.Len(t, seenFiles, n, "one file per upload")
	assert.Len(t, seenBodies, n, "each file holds its own upload")
	assert.Empty(t, srv.Violations())
}

func TestLogLines(t *testing.T) {
	var logged syncBuffer

	srv := fakeserver.Start(t, fakeserver.Options{Log: debugLog(&logged)})

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=pgSessions", "rows")
	post(t, srv, srv.URL()+string(fakeserver.Receiver)+"?dt=pgHealth&de=1&timezoneId=VVRD", "rows")

	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	require.Len(t, lines, 5)

	assert.Regexp(t, `^time=\S+ level=INFO msg=request method=POST path=/ycrash-receiver dt=pgSessions status=200 bytes=4 duration=\S+ auth=ok file=`+run+`/pgSessions.txt reply=OK$`, lines[0])
	assert.Regexp(t, `^time=\S+ level=DEBUG msg="request detail" path=/ycrash-receiver query="de=127.0.0.1&ts=2006-01-02T15-04-05&timezoneId=VVRD&dt=pgSessions" header="Accept-Encoding: gzip; Apikey: <masked>; Content-Length: 4; Content-Type: text; User-Agent: Go-http-client/1.1"$`, lines[1])
	assert.Regexp(t, `^time=\S+ level=INFO msg=request method=POST path=/ycrash-receiver dt=pgHealth status=200 bytes=4 duration=\S+ auth=ok file=no-ts/pgHealth.txt reply=OK$`, lines[2])
	assert.Regexp(t, `^time=\S+ level=DEBUG msg="request detail" path=/ycrash-receiver query="dt=pgHealth&de=1&timezoneId=VVRD" `, lines[3])
	assert.Regexp(t, `^time=\S+ level=WARN msg=violation rule=query path=/ycrash-receiver dt=pgHealth detail="missing ts"$`, lines[4])
}
