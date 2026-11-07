package ondemand

import (
	"net/http"
	"testing"
	"time"

	"yc-agent/internal/config"
	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useFakeServer points the agent at srv until the test ends.
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

func TestRequestFinReadsTheReportURL(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})
	useFakeServer(t, srv)

	resp, err := RequestFin(srv.URLFor(fakeserver.Fin))
	require.NoError(t, err)

	reportURL, result := printResult(true, "1s", resp)
	assert.Equal(t, srv.URL()+"/report", reportURL)
	assert.Contains(t, result, "dashboardReportURL")
	assert.Empty(t, srv.Violations())
}

// RequestFin returns the body of any reply, so a failed yc-fin that carries a
// JSON body reads like a successful one.
func TestRequestFinIgnoresTheStatusCode(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Fin: {Status: http.StatusInternalServerError, Body: new(`{"dashboardReportURL":"https://report.example/1"}`)},
	}})
	useFakeServer(t, srv)

	resp, err := RequestFin(srv.URLFor(fakeserver.Fin))
	require.NoError(t, err, "a 500 is not an error to RequestFin")

	reportURL, _ := printResult(true, "1s", resp)
	assert.Equal(t, "https://report.example/1", reportURL, "the 500's body is read as the report")
	assert.Empty(t, srv.Violations())
}

// A refused key reaches the caller as an empty reply: no error, and no result
// table, but nothing that says the key was refused.
func TestRequestFinReadsARefusedKeyAsAnEmptyReply(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})
	useFakeServer(t, srv)
	config.GlobalConfig.ApiKey = "other@00000000-0000-0000-0000-000000000000"

	resp, err := RequestFin(srv.URLFor(fakeserver.Fin))
	require.NoError(t, err, "the refusal is not reported as an error")
	assert.Empty(t, resp)

	reportURL, result := printResult(false, "1s", resp)
	assert.Empty(t, reportURL)
	assert.Empty(t, result, "no result table")

	assert.Equal(t, fakeserver.AuthMismatch, srv.Requests()[0].Auth)

	for _, v := range srv.Violations() {
		assert.Equal(t, fakeserver.RuleAuth, v.Rule, "the refused key is all that is wrong: %s", v)
	}
}

func TestRequestFinOnADroppedConnectionIsAnError(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{Replies: map[fakeserver.Endpoint]fakeserver.Reply{
		fakeserver.Fin: {Drop: true},
	}})
	useFakeServer(t, srv)

	resp, err := RequestFin(srv.URLFor(fakeserver.Fin))
	assert.Error(t, err)
	assert.Empty(t, resp)
	assert.Empty(t, srv.Violations())
}

func TestRequestFinOverTLS(t *testing.T) {
	for _, tt := range []struct {
		name      string
		useCA     bool
		verifySSL bool
		wantErr   bool
	}{
		{"verified through caCertPath", true, true, false},
		{"not verified, with verifySSL false", false, false, false},
		{"refused with neither", false, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeserver.Start(t, fakeserver.Options{TLS: true})
			useFakeServer(t, srv)

			config.GlobalConfig.VerifySSL = tt.verifySSL
			if !tt.useCA {
				config.GlobalConfig.CACertPath = ""
			}

			resp, err := RequestFin(srv.URLFor(fakeserver.Fin))
			assert.Empty(t, srv.Violations())

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "certificate")

				return
			}

			require.NoError(t, err)

			reportURL, _ := printResult(true, "1s", resp)
			assert.Equal(t, srv.URL()+"/report", reportURL)
		})
	}
}
