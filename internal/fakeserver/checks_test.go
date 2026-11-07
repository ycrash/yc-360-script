package fakeserver_test

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnUnknownPathIs404AndOnlyAPathViolation(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	req, err := http.NewRequest(http.MethodGet, srv.URL()+"/report", nil)
	require.NoError(t, err)

	status, reply := send(t, req)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Empty(t, reply)

	assert.Equal(t, []string{"path: not an endpoint the agent requests"}, rules(srv.Violations()),
		"the checks of the six endpoints do not apply to a path outside them")
	assert.Equal(t, fakeserver.AuthMissing, srv.Requests()[0].Auth)
}

func TestMethodIsCheckedAndTheReplyIsAsSet(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	status, reply := send(t, agentRequest(t, srv, http.MethodGet, srv.URLFor(fakeserver.Receiver)+"&dt=td", ""))
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "OK", reply)

	status, _ = post(t, srv, srv.URLFor(fakeserver.Attendance), "")
	assert.Equal(t, http.StatusOK, status)

	assert.Equal(t, []string{
		"method: GET, want POST",
		"method: POST, want GET",
	}, rules(srv.Violations()))
}

func TestAuth(t *testing.T) {
	const wrongKey = "other@00000000-0000-0000-0000-000000000000"

	for _, tt := range []struct {
		name       string
		header     []string // nil sends no header
		wantStatus int
		wantAuth   string
		wantRules  []string
	}{
		{"the right key", []string{fakeserver.DefaultAPIKey}, http.StatusOK, fakeserver.AuthOK, nil},
		{"no header", nil, http.StatusUnauthorized, fakeserver.AuthMissing, []string{"auth: no ApiKey header"}},
		{"an empty header", []string{""}, http.StatusUnauthorized, fakeserver.AuthMissing, []string{"auth: the ApiKey header is empty"}},
		{"another key", []string{wrongKey}, http.StatusUnauthorized, fakeserver.AuthMismatch, []string{"auth: the ApiKey header does not match"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeserver.Start(t, fakeserver.Options{})

			req := agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
			req.Header.Del("ApiKey")
			for _, v := range tt.header {
				req.Header.Add("ApiKey", v)
			}

			status, reply := send(t, req)
			assert.Equal(t, tt.wantStatus, status)

			if tt.wantStatus != http.StatusOK {
				assert.Empty(t, reply, "a refused key gets an empty body")
			}

			assert.Equal(t, tt.wantAuth, srv.Requests()[0].Auth)

			var want []string
			want = append(want, tt.wantRules...)
			assert.Equal(t, want, nilIfEmpty(rules(srv.Violations())))
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}

	return s
}

func TestARefusedKeyGetsAuthStatusAtOnce(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{
		APIKey: "team@00000000-0000-0000-0000-000000000000",
		Replies: map[fakeserver.Endpoint]fakeserver.Reply{
			fakeserver.Receiver: {AuthStatus: http.StatusForbidden},
			fakeserver.Fin:      {Drop: true},
		},
	})

	req := agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
	req.Header.Set("ApiKey", "other@00000000-0000-0000-0000-000000000000")

	status, _ := send(t, req)
	assert.Equal(t, http.StatusForbidden, status)

	req = agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Fin), "")
	req.Header.Del("ApiKey")

	status, _ = send(t, req)
	assert.Equal(t, http.StatusUnauthorized, status, "a refused key is answered, not dropped")
}

func TestContentTypeOnEveryPost(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	req := agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
	req.Header.Del("Content-Type")
	send(t, req)

	req = agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Fin), "")
	req.Header.Set("Content-Type", "application/json")
	send(t, req)

	send(t, agentRequest(t, srv, http.MethodGet, srv.URLFor(fakeserver.Attendance), ""))

	assert.Equal(t, []string{
		`content-type: Content-Type "", want "text"`,
		`content-type: Content-Type "application/json", want "text"`,
	}, rules(srv.Violations()), "a GET carries no body and needs no Content-Type")
}

func TestQueryParametersTheAgentAlwaysSends(t *testing.T) {
	const tz = "VVRD"

	for _, tt := range []struct {
		name  string
		path  fakeserver.Endpoint
		query string
		want  []string
	}{
		{"an on-demand upload", fakeserver.Receiver, "de=1&ts=t&timezoneId=" + tz + "&dt=td", nil},
		{"no ts", fakeserver.Receiver, "de=1&timezoneId=" + tz + "&dt=td", []string{"query: missing ts"}},
		{"an empty value", fakeserver.Receiver, "de=&ts=t&timezoneId=" + tz + "&dt=td", []string{"query: missing de"}},
		{"no dt on an upload", fakeserver.HeapReceiver, "de=1&ts=t&timezoneId=" + tz, []string{"query: missing dt"}},
		{"the M3 spelling on an on-demand path", fakeserver.Receiver, "de=1&ts=t&timezoneID=" + tz + "&dt=td",
			[]string{"query: missing timezoneId (sent as timezoneID)"}},
		{"the on-demand spelling on an M3 path", fakeserver.M3Receiver, "de=1&ts=t&timezoneId=" + tz + "&dt=gc",
			[]string{"query: missing timezoneID (sent as timezoneId)"}},
		{"yc-fin takes no dt", fakeserver.Fin, "de=1&ts=t&timezoneId=" + tz, nil},
		{"m3-fin needs cpuCount", fakeserver.M3Fin, "de=1&ts=t&timezoneID=" + tz, []string{"query: missing cpuCount"}},
		{"attendance needs type", fakeserver.Attendance, "de=1&ts=t", []string{"query: missing type"}},
		{"several at once", fakeserver.M3Fin, "ts=t", []string{"query: missing de, timezoneID, cpuCount"}},
		{"parameters sent only sometimes are not required", fakeserver.M3Receiver,
			"de=1&ts=t&timezoneID=" + tz + "&dt=gc&pid=4711&cpuCount=8&logName=app&Content-Encoding=zst", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeserver.Start(t, fakeserver.Options{})

			method := http.MethodPost
			if tt.path == fakeserver.Attendance {
				method = http.MethodGet
			}

			send(t, agentRequest(t, srv, method, srv.URL()+string(tt.path)+"?"+tt.query, "x"))

			assert.Equal(t, tt.want, nilIfEmpty(rules(srv.Violations())))
		})
	}
}

func TestACustomCommandUploadsWithoutDT(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{CustomParams: []string{"tp=pidstat"}})

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&tp=pidstat", "custom output")
	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&tp=vmstat", "not a configured command")

	assert.Equal(t, []string{"query: missing dt"}, rules(srv.Violations()))
	assert.Equal(t, 2, srv.Violations()[0].Seq, "the violation names the second request")
}

func TestTheKeyMustNotBeInTheURL(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})
	key := srv.APIKey()

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td&apiKey="+url.QueryEscape(key), "x")
	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td&apiKey="+key, "x")
	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=ps", "UID PID CMD\n501 4711 yc -k "+key+"\n")

	assert.Equal(t, []string{
		"key-leak: the API key is in the URL",
		"key-leak: the API key is in the URL",
	}, rules(srv.Violations()), "a body may carry the key: the agent's ps capture shows its own command line")

	r := srv.Requests()[0]
	assert.Equal(t, "<api-key>", r.Query.Get("apiKey"), "the record masks the key")
	assert.Equal(t, []string{"<masked>"}, r.Header.Values("ApiKey"))
}

func TestSecretsMustNotReachTheServer(t *testing.T) {
	secrets := []string{"s3cr3t pw", "", "other-secret"}
	srv := fakeserver.Start(t, fakeserver.Options{Secrets: secrets})

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td&note="+url.QueryEscape(secrets[0]), "x")
	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td&note="+url.PathEscape(secrets[0]), "x")

	req := agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Receiver)+"&dt=td", "x")
	req.Header.Set("X-Debug", "password="+secrets[2])
	send(t, req)

	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=pgSessions", "query,ALTER ROLE app PASSWORD '"+secrets[0]+"'\n")
	post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=pgHealth", "nothing to see")

	assert.Equal(t, []string{
		"secret-leak: secret 1 is in the URL",
		"secret-leak: secret 1 is in the URL",
		"secret-leak: secret 3 is in the X-Debug header",
		"secret-leak: secret 1 is in the body",
	}, rules(srv.Violations()), "an empty secret matches nothing")
}

func TestAnEncodedBodyIsScannedAsSent(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Secrets: []string{"s3cr3t"}})

	encoded := base64.StdEncoding.EncodeToString([]byte("heap with s3cr3t inside"))
	post(t, srv, srv.URLFor(fakeserver.HeapReceiver)+"&dt=hd&Content-Encoding=b64", encoded)

	assert.Empty(t, srv.Violations(), "the fake does not decode a body")
}

func TestTheKeyAndSecretsStayOutOfTheRecords(t *testing.T) {
	const secret = "s3cr3t-pw"

	var logged syncBuffer

	srv := fakeserver.Start(t, fakeserver.Options{Secrets: []string{secret}, Log: debugLog(&logged)})
	key := srv.APIKey()

	req := agentRequest(t, srv, http.MethodPost,
		srv.URLFor(fakeserver.Receiver)+"&dt=td&apiKey="+url.QueryEscape(key)+"&pw="+secret, "body with "+secret)
	req.Header.Set("X-Debug", secret)
	send(t, req)

	require.Len(t, srv.Violations(), 4)

	jsonl, err := os.ReadFile(filepath.Join(srv.Dir(), "requests.jsonl"))
	require.NoError(t, err)

	text, err := os.ReadFile(filepath.Join(srv.Dir(), "yc-2006-01-02T15-04-05", "requests.txt"))
	require.NoError(t, err)

	for name, content := range map[string]string{
		"the log":        logged.String(),
		"requests.jsonl": string(jsonl),
		"requests.txt":   string(text),
	} {
		for _, needle := range []string{key, url.QueryEscape(key), secret} {
			assert.NotContains(t, content, needle, name)
		}
	}

	assert.Contains(t, logged.String(), "pw=<secret-1>")

	body, err := srv.Requests()[0].Body()
	require.NoError(t, err)
	assert.Contains(t, string(body), secret, "the stored body is what arrived")
}
