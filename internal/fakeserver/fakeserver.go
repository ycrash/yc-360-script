// Package fakeserver stands in for the yCrash server. It records every request
// the agent makes, checks each one against what the agent is meant to send, and
// replies however the test sets it up. It knows the server's endpoints by name
// only, so it says nothing about how the real server would treat a request.
//
// A unit test starts one with Start, points the agent at URL and APIKey, and
// ends with require.Empty(t, srv.Violations()). cmd/yc-fake-server serves the
// same handler from a shell.
package fakeserver

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Endpoint is a path the agent requests on the server.
type Endpoint string

const (
	Receiver     Endpoint = "/ycrash-receiver"
	HeapReceiver Endpoint = "/yc-receiver-heap"
	Fin          Endpoint = "/yc-fin"
	M3Receiver   Endpoint = "/m3-receiver"
	M3Fin        Endpoint = "/m3-fin"
	Attendance   Endpoint = "/yc-attendance"
)

// Endpoints lists every endpoint the agent requests.
var Endpoints = []Endpoint{Receiver, HeapReceiver, Fin, M3Receiver, M3Fin, Attendance}

// endpointShape is what the agent sends on every request to one endpoint.
type endpointShape struct {
	method string
	params []string
	upload bool // carries an artifact, so dt is required as well
}

// On-demand paths spell the time zone timezoneId and M3 paths timezoneID; each
// is required the way the agent sends it today.
var endpointShapes = map[Endpoint]endpointShape{
	Receiver:     {http.MethodPost, []string{"de", "ts", "timezoneId"}, true},
	HeapReceiver: {http.MethodPost, []string{"de", "ts", "timezoneId"}, true},
	Fin:          {http.MethodPost, []string{"de", "ts", "timezoneId"}, false},
	M3Receiver:   {http.MethodPost, []string{"de", "ts", "timezoneID"}, true},
	M3Fin:        {http.MethodPost, []string{"de", "ts", "timezoneID", "cpuCount"}, false},
	Attendance:   {http.MethodGet, []string{"type", "de", "ts"}, false},
}

// ParseEndpoint accepts an endpoint's path with or without its leading slash.
func ParseEndpoint(name string) (Endpoint, bool) {
	e := Endpoint("/" + strings.TrimPrefix(name, "/"))
	_, ok := endpointShapes[e]

	return e, ok
}

// DefaultAPIKey is the key the fake expects when Options.APIKey is empty.
const DefaultAPIKey = "fake-server@00000000-0000-0000-0000-000000000000"

// Reply is how the fake answers one endpoint. The zero value is the default.
type Reply struct {
	Status     int           // 0 answers 200
	Body       *string       // nil answers the endpoint's default body
	Delay      time.Duration // waited before answering, or until the client gives up
	Drop       bool          // closes the connection instead of answering
	AuthStatus int           // the answer to a missing or wrong key; 0 answers 401
}

// Options sets up a fake server.
type Options struct {
	// APIKey is the value every request must carry in its ApiKey header.
	APIKey string
	// Secrets must never reach the server: in a URL, a header or a body.
	Secrets []string
	Replies map[Endpoint]Reply
	// FinActions answers successive /m3-fin calls with one list each; the last repeats.
	FinActions [][]string
	// CustomParams are the urlParams of configured custom commands, whose uploads carry no dt.
	CustomParams []string
	// TLS serves HTTPS with a certificate from a CA written to CACertPath.
	TLS bool
	// TLSHosts are names or addresses the certificate covers besides localhost.
	TLSHosts []string
	// Dir receives one file per request body, grouped by the request's ts. A fake
	// started again on the same Dir numbers its requests on from the last one there.
	Dir string
	// NoBodies keeps only each body's size and SHA-256.
	NoBodies bool
	// Log receives the request, violation and cut-off lines; nil discards them.
	Log *slog.Logger
}

// Server records and checks requests. Its methods are safe for concurrent use.
type Server struct {
	opts    Options
	key     string
	masker  *masker
	custom  []url.Values
	tlsConf *tls.Config
	caPath  string
	url     string

	mu       sync.Mutex
	seq      int
	requests []Request
	names    map[string]int
	finCalls int
}

// New prepares a server that writes what it receives under opts.Dir.
func New(opts Options) (*Server, error) {
	if opts.Dir == "" {
		return nil, errors.New("fakeserver: Options.Dir is empty")
	}

	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("fakeserver: %w", err)
	}

	for e, reply := range opts.Replies {
		for _, status := range []int{reply.Status, reply.AuthStatus} {
			// A 1xx is not a final reply: net/http would send its own 200 after it.
			if status != 0 && (status < 200 || status > 599) {
				return nil, fmt.Errorf("fakeserver: %s: %d is not a final HTTP status", e, status)
			}
		}
	}

	seq, err := lastSeq(filepath.Join(opts.Dir, requestsJSONL))
	if err != nil {
		return nil, fmt.Errorf("fakeserver: %w", err)
	}

	s := &Server{opts: opts, key: opts.APIKey, names: map[string]int{}, seq: seq}
	if s.key == "" {
		s.key = DefaultAPIKey
	}

	s.masker = newMasker(s.key, opts.Secrets)

	for _, params := range opts.CustomParams {
		values, err := url.ParseQuery(params)
		if err != nil {
			return nil, fmt.Errorf("fakeserver: custom params %q: %w", params, err)
		}

		s.custom = append(s.custom, values)
	}

	if opts.TLS {
		caPEM, cert, err := newCertificates(opts.TLSHosts)
		if err != nil {
			return nil, fmt.Errorf("fakeserver: %w", err)
		}

		s.caPath = filepath.Join(opts.Dir, "ca.pem")
		if err := os.WriteFile(s.caPath, caPEM, 0o600); err != nil {
			return nil, fmt.Errorf("fakeserver: %w", err)
		}

		s.tlsConf = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}

	return s, nil
}

// Start serves a new fake on a free loopback port until the test ends. Without
// opts.Dir, bodies go to a directory the test removes afterwards.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()

	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}

	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}

	hs := httptest.NewUnstartedServer(s.Handler())
	if s.tlsConf != nil {
		hs.TLS = s.tlsConf
		hs.StartTLS()
	} else {
		hs.Start()
	}

	s.url = hs.URL
	t.Cleanup(hs.Close)

	return s
}

// Handler serves the six endpoints and records every request, known or not.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// URL is the base URL Start serves on, for the agent's -s.
func (s *Server) URL() string { return s.url }

// APIKey is the key requests must carry.
func (s *Server) APIKey() string { return s.key }

// Dir is where bodies, requests.txt and requests.jsonl are written.
func (s *Server) Dir() string { return s.opts.Dir }

// CACertPath is the CA that signed the server's certificate, for the agent's
// caCertPath; empty without TLS.
func (s *Server) CACertPath() string { return s.caPath }

// TLSConfig is the server side of TLS, for serving Handler yourself; nil without TLS.
func (s *Server) TLSConfig() *tls.Config { return s.tlsConf }

// urlForTS is the ts URLFor sends. Go's reference time reads as a placeholder.
const urlForTS = "2006-01-02T15-04-05"

// URLFor is e's URL with the parameters the agent always sends on it, for a test
// that calls an upload helper directly: the helpers append &dt=<type>.
func (s *Server) URLFor(e Endpoint) string {
	utc := base64.StdEncoding.EncodeToString([]byte("UTC"))

	var query string

	switch e {
	case M3Receiver:
		query = "de=127.0.0.1&ts=" + urlForTS + "&timezoneID=" + utc
	case M3Fin:
		query = "de=127.0.0.1&ts=" + urlForTS + "&timezoneID=" + utc + "&cpuCount=1"
	case Attendance:
		query = "type=startup&de=127.0.0.1&ts=" + urlForTS
	default:
		query = "de=127.0.0.1&ts=" + urlForTS + "&timezoneId=" + utc
	}

	return s.url + string(e) + "?" + query
}

// Requests returns every request answered so far, in the order they arrived.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })

	return out
}

// Violations returns every failed check so far, in the order the requests arrived.
func (s *Server) Violations() []Violation {
	var out []Violation
	for _, r := range s.Requests() {
		out = append(out, r.Violations...)
	}

	return out
}
