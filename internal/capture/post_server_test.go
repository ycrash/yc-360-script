package capture

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yc-agent/internal/config"
	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useFakeServer points the upload helpers at srv until the test ends.
func useFakeServer(t *testing.T, srv *fakeserver.Server) {
	t.Helper()

	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })

	config.GlobalConfig.OnlyCapture = false
	config.GlobalConfig.Server = srv.URL()
	config.GlobalConfig.ApiKey = srv.APIKey()
	config.GlobalConfig.VerifySSL = true
	config.GlobalConfig.CACertPath = srv.CACertPath()
	config.GlobalConfig.HttpClientTimeout = config.Duration(10 * time.Second)
}

func artifactFile(t *testing.T, content string) *os.File {
	t.Helper()

	f, err := os.Create(filepath.Join(t.TempDir(), "artifact.out"))
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })

	_, err = f.WriteString(content)
	require.NoError(t, err)

	return f
}

func TestPostDataSendsWhatTheServerChecks(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})
	useFakeServer(t, srv)

	msg, ok := PostData(srv.URLFor(fakeserver.Receiver), "td", artifactFile(t, "Full thread dump"))
	require.True(t, ok, msg)

	msg, ok = GetData(srv.URLFor(fakeserver.Attendance))
	require.True(t, ok, msg)

	assert.Empty(t, srv.Violations())

	body, err := srv.Requests()[0].Body()
	require.NoError(t, err)
	assert.Equal(t, "Full thread dump", string(body))
}

func TestAnUploadWithARefusedKeyIsNotOk(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})
	useFakeServer(t, srv)
	config.GlobalConfig.ApiKey = "other@00000000-0000-0000-0000-000000000000"

	msg, ok := PostData(srv.URLFor(fakeserver.Receiver), "td", artifactFile(t, "x"))
	assert.False(t, ok)
	assert.Contains(t, msg, "status code 401")

	msg, ok = GetData(srv.URLFor(fakeserver.Attendance))
	assert.False(t, ok)
	assert.Contains(t, msg, "status code 401")

	for _, r := range srv.Requests() {
		assert.Equal(t, fakeserver.AuthMismatch, r.Auth, r.Path)
	}

	for _, v := range srv.Violations() {
		assert.Equal(t, fakeserver.RuleAuth, v.Rule, "the refused key is all that is wrong: %s", v)
	}
}

// Each upload helper sets up TLS on its own, so each is tried against the same
// three servers.
func TestUploadsOverTLS(t *testing.T) {
	helpers := map[string]func(srv *fakeserver.Server) (string, bool){
		"PostData": func(srv *fakeserver.Server) (string, bool) {
			return PostData(srv.URLFor(fakeserver.Receiver), "td", artifactFile(t, "x"))
		},
		"PostReaderWithTimeout": func(srv *fakeserver.Server) (string, bool) {
			return PostReaderWithTimeout(srv.URLFor(fakeserver.HeapReceiver), "hd&Content-Encoding=zst",
				strings.NewReader("streamed"), 0)
		},
		"GetData": func(srv *fakeserver.Server) (string, bool) {
			return GetData(srv.URLFor(fakeserver.Attendance))
		},
	}

	for _, tt := range []struct {
		name      string
		useCA     bool
		verifySSL bool
		wantOK    bool
	}{
		{"verified through caCertPath", true, true, true},
		{"not verified, with verifySSL false", false, false, true},
		{"refused with neither", false, true, false},
	} {
		for name, upload := range helpers {
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				srv := fakeserver.Start(t, fakeserver.Options{TLS: true})
				useFakeServer(t, srv)

				config.GlobalConfig.VerifySSL = tt.verifySSL
				if !tt.useCA {
					config.GlobalConfig.CACertPath = ""
				}

				msg, ok := upload(srv)
				assert.Equal(t, tt.wantOK, ok, msg)

				if tt.wantOK {
					assert.Len(t, srv.Requests(), 1)
					assert.Empty(t, srv.Violations())
				} else {
					assert.Contains(t, msg, "certificate")
					assert.Empty(t, srv.Requests(), "the handshake fails before a request")
				}
			})
		}
	}
}

func TestAnUploadOnADroppedConnectionIsNotOk(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Receiver: {Drop: true},
	}})
	useFakeServer(t, srv)

	msg, ok := PostData(srv.URLFor(fakeserver.Receiver), "td", artifactFile(t, "x"))
	assert.False(t, ok)
	assert.Contains(t, msg, "PostData post err")

	require.Len(t, srv.Requests(), 1)
	assert.Equal(t, int64(1), srv.Requests()[0].Bytes, "the body arrived before the connection closed")
	assert.Empty(t, srv.Violations())
}

func TestAnUploadPastTheClientTimeoutIsNotOk(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Receiver: {Delay: time.Minute},
	}})
	useFakeServer(t, srv)
	config.GlobalConfig.HttpClientTimeout = config.Duration(200 * time.Millisecond)

	start := time.Now()
	msg, ok := PostData(srv.URLFor(fakeserver.Receiver), "td", artifactFile(t, "x"))
	assert.False(t, ok)
	assert.Contains(t, msg, "Client.Timeout exceeded")
	assert.Less(t, time.Since(start), 10*time.Second, "httpClientTimeout bounds the wait")

	// The fake records the request once the client has hung up.
	require.Eventually(t, func() bool { return len(srv.Requests()) == 1 }, 10*time.Second, 10*time.Millisecond)
	assert.Empty(t, srv.Violations())
}

// Each upload helper writes its TLS settings into http.DefaultTransport, which races
// under -race; without it, the test shows parallel uploads each arrive whole.
func TestParallelUploadsEachArriveWhole(t *testing.T) {
	if raceEnabled {
		t.Skip("each upload helper writes its TLS settings into http.DefaultTransport, which races with an upload beside it")
	}

	srv := fakeserver.Start(t, fakeserver.Options{})
	useFakeServer(t, srv)

	const n = 8

	var wg sync.WaitGroup

	results := make([]bool, n)

	for i := range n {
		wg.Go(func() {
			_, results[i] = PostData(srv.URLFor(fakeserver.Receiver), fmt.Sprintf("dt%d", i), artifactFile(t, fmt.Sprintf("body %d", i)))
		})
	}

	wg.Wait()

	for i, ok := range results {
		assert.True(t, ok, "upload %d", i)
	}

	requests := srv.Requests()
	require.Len(t, requests, n)

	for _, r := range requests {
		body, err := r.Body()
		require.NoError(t, err)
		assert.Equal(t, "body "+strings.TrimPrefix(r.DT, "dt"), string(body))
		assert.Equal(t, http.StatusOK, r.Status)
	}

	assert.Empty(t, srv.Violations())
}
