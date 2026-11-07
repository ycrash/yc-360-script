package m3

import (
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cyclePassword = "s3cr3t-do-not-log"

// monitorUnreachableDatabase monitors a port nothing listens on, against srv, from
// a temporary directory: a cycle then runs end to end with no database or process.
func monitorUnreachableDatabase(t *testing.T, srv *fakeserver.Server) {
	t.Helper()

	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	window := config.Duration(time.Second)

	config.GlobalConfig = config.Config{}
	config.GlobalConfig.M3 = true
	config.GlobalConfig.Server = srv.URL()
	config.GlobalConfig.ApiKey = srv.APIKey()
	config.GlobalConfig.VerifySSL = true
	config.GlobalConfig.HttpClientTimeout = config.Duration(10 * time.Second)
	config.GlobalConfig.Postgres = &config.Postgres{DBTarget: config.DBTarget{
		ID: "orders-primary", Host: "127.0.0.1", Port: port, Database: "orders_db",
		Username: "ycrash_monitor", Password: cyclePassword, CaptureDuration: &window,
	}}

	dir := t.TempDir()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(cwd) })
}

func uploadsTo(srv *fakeserver.Server, e fakeserver.Endpoint) map[string]int {
	dts := map[string]int{}

	for _, r := range srv.Requests() {
		if r.Path == string(e) {
			dts[r.DT]++
		}
	}

	return dts
}

// A refused /m3-fin carries an empty body, which the cycle skips as an empty
// reply: the cycle succeeds, and nothing says the key was refused.
func TestAnM3CycleReadsARefusedKeyAsAnEmptyReply(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})
	monitorUnreachableDatabase(t, srv)
	config.GlobalConfig.ApiKey = "other@00000000-0000-0000-0000-000000000000"

	assert.NoError(t, NewM3App().RunSingle())

	requests := srv.Requests()
	require.Len(t, requests, 2, "the cycle's reading, then its fin call")

	assert.Equal(t, string(fakeserver.M3Receiver), requests[0].Path)
	assert.Equal(t, "pgM3", requests[0].DT)
	assert.Equal(t, string(fakeserver.M3Fin), requests[1].Path)

	for _, r := range requests {
		assert.Equal(t, http.StatusUnauthorized, r.Status, r.Path)
	}

	for _, v := range srv.Violations() {
		assert.Equal(t, fakeserver.RuleAuth, v.Rule, "the refused key is all that is wrong: %s", v)
	}
}

func TestAnM3CycleRunsTheDatabaseCaptureTheServerAsksFor(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{
		FinActions: [][]string{{"capture db"}},
		Secrets:    []string{cyclePassword},
	})
	monitorUnreachableDatabase(t, srv)

	// No ping: it would upload beside the database capture, and parallel uploads
	// share the transport's TLS settings.
	previousPing := executils.Ping
	executils.Ping = nil
	t.Cleanup(func() { executils.Ping = previousPing })

	require.NoError(t, NewM3App().RunSingle())

	assert.Equal(t, map[string]int{"pgM3": 1}, uploadsTo(srv, fakeserver.M3Receiver), "the cycle's own reading")
	assert.Equal(t, map[string]int{"": 1}, uploadsTo(srv, fakeserver.M3Fin))

	deepDive := uploadsTo(srv, fakeserver.Receiver)
	for _, dt := range []string{"meta", "pgMeta", "pgSessions", "pgHealth", "pgSlowQueries", "pgDeadlocks", "pgErrors"} {
		assert.Equal(t, 1, deepDive[dt], "the deep dive uploads %s once", dt)
	}

	assert.Equal(t, map[string]int{"": 1}, uploadsTo(srv, fakeserver.Fin), "and finishes with yc-fin")
	assert.Empty(t, srv.Violations(), "the password reaches the server nowhere, and every request is as the server expects")
}
