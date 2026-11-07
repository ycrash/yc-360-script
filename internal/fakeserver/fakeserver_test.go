package fakeserver_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentRequest builds a request the way the agent's helpers do: the ApiKey header
// always, and Content-Type text on a POST.
func agentRequest(t *testing.T, srv *fakeserver.Server, method, url, body string) *http.Request {
	t.Helper()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, url, r)
	require.NoError(t, err)

	req.Header.Set("ApiKey", srv.APIKey())
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "text")
	}

	return req
}

// send sends req through client, or http.DefaultClient when none is given.
func send(t *testing.T, req *http.Request, client ...*http.Client) (status int, reply string) {
	t.Helper()

	c := http.DefaultClient
	if len(client) > 0 {
		c = client[0]
	}

	resp, err := c.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(b)
}

func post(t *testing.T, srv *fakeserver.Server, url, body string) (status int, reply string) {
	t.Helper()

	return send(t, agentRequest(t, srv, http.MethodPost, url, body))
}

// syncBuffer collects log output written from the handler's goroutines.
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

func debugLog(out io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func rules(vs []fakeserver.Violation) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Rule + ": " + v.Detail
	}

	return out
}

func TestDefaultRepliesLetARunFinish(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	status, reply := post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", "thread dump")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "OK", reply)

	status, reply = post(t, srv, srv.URLFor(fakeserver.HeapReceiver)+"&dt=hd&Content-Encoding=zst", "heap")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "OK", reply)

	status, reply = post(t, srv, srv.URLFor(fakeserver.Fin), "")
	assert.Equal(t, http.StatusOK, status)

	var fin map[string]string
	require.NoError(t, json.Unmarshal([]byte(reply), &fin), "the agent prints its result table only from JSON")
	assert.Equal(t, srv.URL()+"/report", fin["dashboardReportURL"])

	status, reply = post(t, srv, srv.URLFor(fakeserver.M3Receiver)+"&dt=pgM3", "reading")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "OK", reply)

	status, reply = post(t, srv, srv.URLFor(fakeserver.M3Fin), "")
	assert.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, `{"actions":[]}`, reply)

	status, reply = send(t, agentRequest(t, srv, http.MethodGet, srv.URLFor(fakeserver.Attendance), ""))
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "OK", reply)

	assert.Len(t, srv.Requests(), 6)
	assert.Empty(t, srv.Violations(), "URLFor carries what the agent always sends")
}

func TestRequestsComeBackInArrivalOrder(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	for _, dt := range []string{"a", "b", "c"} {
		post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt="+dt, dt)
	}

	var got []string
	for i, r := range srv.Requests() {
		assert.Equal(t, i+1, r.Seq)
		got = append(got, r.DT)
	}

	assert.Equal(t, []string{"a", "b", "c"}, got)
}

func TestReplyStatusAndBody(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Receiver: {Status: http.StatusInternalServerError},
		fakeserver.Fin:      {Body: new("not json")},
		fakeserver.M3Fin:    {Body: new("")},
	}})

	status, reply := post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, "OK", reply, "a status alone keeps the default body")

	status, reply = post(t, srv, srv.URLFor(fakeserver.Fin), "")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "not json", reply)

	_, reply = post(t, srv, srv.URLFor(fakeserver.M3Fin), "")
	assert.Empty(t, reply, "an empty body is a body, not the default")

	_, reply = post(t, srv, srv.URLFor(fakeserver.M3Receiver)+"&dt=pgM3", "x")
	assert.Equal(t, "OK", reply, "an endpoint with no reply set keeps the default")

	assert.Empty(t, srv.Violations())
}

func TestReplyDelay(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Receiver: {Delay: 200 * time.Millisecond},
	}})

	start := time.Now()
	status, _ := post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
	assert.Equal(t, http.StatusOK, status)
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)

	requests := srv.Requests()
	require.Len(t, requests, 1)
	assert.GreaterOrEqual(t, requests[0].Duration, 200*time.Millisecond)
}

func TestReplyDelayPastTheClientsTimeout(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Fin: {Delay: time.Minute},
	}})

	client := &http.Client{Timeout: 100 * time.Millisecond}
	resp, err := client.Do(agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Fin), ""))
	if err == nil {
		resp.Body.Close()
	}

	require.Error(t, err, "the client gives up first")

	require.Eventually(t, func() bool { return len(srv.Requests()) == 1 }, 5*time.Second, 10*time.Millisecond,
		"the fake stops waiting when the client leaves")

	r := srv.Requests()[0]
	assert.Equal(t, 0, r.Status, "no reply was written")
	assert.Equal(t, "the connection closed before the reply", r.Err)
}

func TestReplyDrop(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Fin: {Drop: true},
	}})

	resp, err := http.DefaultClient.Do(agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Fin), ""))
	if err == nil {
		resp.Body.Close()
	}

	require.Error(t, err, "the connection closes with no reply")

	requests := srv.Requests()
	require.Len(t, requests, 1, "the request is recorded before the connection is dropped")
	assert.Equal(t, 0, requests[0].Status)
	assert.Equal(t, "dropped without a reply", requests[0].Err)
}

func TestFinActionsOneListPerCallAndTheLastRepeats(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{FinActions: [][]string{
		{},
		{"capture db"},
		{"capture 1", "capture 2"},
	}})

	var replies []string
	for range 4 {
		_, reply := post(t, srv, srv.URLFor(fakeserver.M3Fin), "")
		replies = append(replies, reply)
	}

	assert.Equal(t, []string{
		`{"actions":[]}`,
		`{"actions":["capture db"]}`,
		`{"actions":["capture 1","capture 2"]}`,
		`{"actions":["capture 1","capture 2"]}`,
	}, replies)
}

func TestFinBodyOverridesFinActions(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{
		FinActions: [][]string{{"capture db"}},
		Replies:    map[fakeserver.Endpoint]fakeserver.Reply{fakeserver.M3Fin: {Body: new(`{"actions":["capture 7"]}`)}},
	})

	_, reply := post(t, srv, srv.URLFor(fakeserver.M3Fin), "")
	assert.Equal(t, `{"actions":["capture 7"]}`, reply)
}

func TestParseEndpoint(t *testing.T) {
	for _, name := range []string{"yc-fin", "/yc-fin"} {
		e, ok := fakeserver.ParseEndpoint(name)
		assert.True(t, ok, name)
		assert.Equal(t, fakeserver.Fin, e, name)
	}

	_, ok := fakeserver.ParseEndpoint("report")
	assert.False(t, ok)

	for _, e := range fakeserver.Endpoints {
		parsed, ok := fakeserver.ParseEndpoint(string(e))
		assert.True(t, ok, e)
		assert.Equal(t, e, parsed)
	}
}

func TestNewNeedsADirectory(t *testing.T) {
	_, err := fakeserver.New(fakeserver.Options{})
	assert.Error(t, err)
}

func TestNewRefusesAStatusThatIsNotFinal(t *testing.T) {
	for _, reply := range []fakeserver.Reply{{Status: http.StatusSwitchingProtocols}, {AuthStatus: 600}} {
		_, err := fakeserver.New(fakeserver.Options{Dir: t.TempDir(),
			Replies: map[fakeserver.Endpoint]fakeserver.Reply{fakeserver.Fin: reply}})
		assert.Error(t, err, "%+v", reply)
	}
}

func TestABodylessReplyRecordsNoBody(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Receiver: {Status: http.StatusNoContent},
	}})

	status, reply := post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
	assert.Equal(t, http.StatusNoContent, status)
	assert.Empty(t, reply)

	send(t, agentRequest(t, srv, http.MethodHead, srv.URLFor(fakeserver.Attendance), ""))

	for _, r := range srv.Requests() {
		assert.Empty(t, r.Reply, "%s %s: the record holds what the client was sent", r.Method, r.Path)
	}
}

func TestTheDefaultKey(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})
	assert.Equal(t, fakeserver.DefaultAPIKey, srv.APIKey())

	srv = fakeserver.Start(t, fakeserver.Options{APIKey: "team@00000000-0000-0000-0000-000000000000"})
	assert.Equal(t, "team@00000000-0000-0000-0000-000000000000", srv.APIKey())
}
