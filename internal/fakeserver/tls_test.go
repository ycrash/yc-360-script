package fakeserver_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"strings"
	"testing"

	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trusting is a client that trusts only the CA at caPath, as the agent does with caCertPath.
func trusting(t *testing.T, caPath string) *http.Client {
	t.Helper()

	ca, err := os.ReadFile(caPath)
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))

	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func TestTLSIsVerifiedThroughTheCA(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{TLS: true})
	require.True(t, strings.HasPrefix(srv.URL(), "https://"))

	status, reply := send(t, agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Fin), ""), trusting(t, srv.CACertPath()))
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, reply, `"dashboardReportURL":"`+srv.URL()+`/report"`, "the report URL keeps the scheme")

	resp, err := http.DefaultClient.Do(agentRequest(t, srv, http.MethodPost, srv.URLFor(fakeserver.Fin), ""))
	if err == nil {
		resp.Body.Close()
	}

	require.Error(t, err, "a client that does not trust the CA is refused")
	assert.Contains(t, err.Error(), "certificate")

	assert.Len(t, srv.Requests(), 1, "a refused handshake never reaches the handler")
	assert.Empty(t, srv.Violations())
}

func TestTheCertificateCoversTheExtraHosts(t *testing.T) {
	srv, err := fakeserver.New(fakeserver.Options{Dir: t.TempDir(), TLS: true, TLSHosts: []string{"yc-fake.test", "10.1.2.3"}})
	require.NoError(t, err)

	caPEM, err := os.ReadFile(srv.CACertPath())
	require.NoError(t, err)

	block, _ := pem.Decode(caPEM)
	require.NotNil(t, block)

	ca, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.True(t, ca.IsCA)

	leaf, err := x509.ParseCertificate(srv.TLSConfig().Certificates[0].Certificate[0])
	require.NoError(t, err)

	pool := x509.NewCertPool()
	pool.AddCert(ca)

	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool})
	require.NoError(t, err, "the CA signed the server's certificate")

	for _, host := range []string{"localhost", "127.0.0.1", "::1", "yc-fake.test", "10.1.2.3"} {
		assert.NoError(t, leaf.VerifyHostname(host), host)
	}

	assert.Error(t, leaf.VerifyHostname("elsewhere.test"))
}

func TestNoTLSNoCA(t *testing.T) {
	srv := fakeserver.Start(t, fakeserver.Options{})

	assert.Empty(t, srv.CACertPath())
	assert.Nil(t, srv.TLSConfig())
	assert.True(t, strings.HasPrefix(srv.URL(), "http://"))
}
