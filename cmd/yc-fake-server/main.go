// Command yc-fake-server stands in for the yCrash server, for tests and for
// running the agent by hand. Run yc-fake-server -h for its flags.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"yc-agent/internal/fakeserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// A second signal ends the process at once instead of waiting for uploads.
	go func() {
		<-ctx.Done()
		stop()
	}()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usageHeader = `Usage: yc-fake-server [flags]

A stand-in for the yCrash server. Point the agent at it with -s and -k: every
request is checked and logged on stdout, and every body is written under -out,
in one yc-<ts> directory per run, as the agent names its capture directory.

Endpoints, for -status, -delay and -drop:
  %s.

Flags:
`

// config is what the flags ask for.
type config struct {
	addr         string
	apiKey       string
	finActions   [][]string
	customParams []string
	secretsFile  string
	replies      map[fakeserver.Endpoint]fakeserver.Reply
	tls          bool
	tlsHosts     []string
	out          string
	noBodies     bool
	logFormat    string
	logLevel     slog.Level
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}

	if err != nil {
		fmt.Fprintf(stderr, "yc-fake-server: %v\n", err)

		return 2
	}

	secrets, err := readSecrets(cfg.secretsFile)
	if err != nil {
		fmt.Fprintf(stderr, "yc-fake-server: %v\n", err)

		return 2
	}

	// Listen first, so a second fake on a busy port writes nothing, not even ca.pem.
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		fmt.Fprintf(stderr, "yc-fake-server: %v\n", err)

		return 1
	}

	defer ln.Close()

	out := &lockedWriter{w: stdout}
	// Startup and shutdown lines show at every -log-level.
	always := newLogger(out, cfg.logFormat, slog.LevelInfo)

	srv, err := fakeserver.New(fakeserver.Options{
		APIKey:       cfg.apiKey,
		Secrets:      secrets,
		Replies:      cfg.replies,
		FinActions:   cfg.finActions,
		CustomParams: cfg.customParams,
		TLS:          cfg.tls,
		TLSHosts:     cfg.tlsHosts,
		Dir:          cfg.out,
		NoBodies:     cfg.noBodies,
		Log:          newLogger(out, cfg.logFormat, cfg.logLevel),
	})
	if err != nil {
		fmt.Fprintf(stderr, "yc-fake-server: %v\n", err)

		return 1
	}

	logStartup(always, cfg, ln.Addr().String())

	var inFlight sync.RWMutex

	hs := &http.Server{
		Handler:           counted(srv.Handler(), &inFlight),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(stderr, "yc-fake-server: ", 0),
	}

	if cfg.tls {
		hs.TLSConfig = srv.TLSConfig()
		// HTTP/1.1 only, so a dropped reply can close the connection.
		hs.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}

	served := make(chan error, 1)

	go func() {
		if cfg.tls {
			served <- hs.ServeTLS(ln, "", "")
		} else {
			served <- hs.Serve(ln)
		}
	}()

	select {
	case err := <-served:
		fmt.Fprintf(stderr, "yc-fake-server: %v\n", err)

		return 1
	case <-ctx.Done():
	}

	// Let uploads in flight finish for a moment, then cut them off; either way
	// every request is recorded before the counts are logged.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := hs.Shutdown(shutdown); err != nil {
		hs.Close()
	}

	cancel()
	inFlight.Lock()

	always.Info("stopped", "requests", len(srv.Requests()), "violations", len(srv.Violations()))

	return 0
}

func logStartup(logger *slog.Logger, cfg config, addr string) {
	// The default key is a public fixture, shown so the agent's -k can be copied from
	// it; a key given with -api-key may be a real one.
	key := "<masked>"
	if cfg.apiKey == fakeserver.DefaultAPIKey {
		key = cfg.apiKey
	}

	logger.Info("listening", "addr", addr, "out", cfg.out, "tls", cfg.tls, "api_key", key)

	for _, e := range fakeserver.Endpoints {
		reply, ok := cfg.replies[e]
		if !ok {
			continue
		}

		status, authStatus := reply.Status, reply.AuthStatus
		if status == 0 {
			status = http.StatusOK
		}

		if authStatus == 0 {
			authStatus = http.StatusUnauthorized
		}

		logger.Info("reply set", "path", string(e), "status", status, "delay", reply.Delay,
			"drop", reply.Drop, "auth_status", authStatus)
	}

	for i, actions := range cfg.finActions {
		logger.Info("fin actions", "call", i+1, "actions", strings.Join(actions, ","),
			"repeats", i == len(cfg.finActions)-1)
	}
}

// counted holds a read lock while a request is served, so shutdown waits for
// every record by taking the write lock; a request that starts later waits behind it.
func counted(h http.Handler, inFlight *sync.RWMutex) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.RLock()
		defer inFlight.RUnlock()

		h.ServeHTTP(w, r)
	})
}

func newLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}

	return slog.New(slog.NewTextHandler(w, opts))
}

// lockedWriter keeps the two loggers' lines whole on one stdout.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.w.Write(p)
}

// readSecrets reads one secret per line. Read from a file, not a flag, because
// the agent uploads ps output, which shows every command line on the host.
func readSecrets(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-secrets-file: %w", err)
	}

	var secrets []string

	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSuffix(line, "\r"); line != "" {
			secrets = append(secrets, line)
		}
	}

	if len(secrets) == 0 {
		return nil, fmt.Errorf("-secrets-file %s holds no secrets", path)
	}

	return secrets, nil
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	cfg := config{replies: map[fakeserver.Endpoint]fakeserver.Reply{}}

	fs := flag.NewFlagSet("yc-fake-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		names := make([]string, len(fakeserver.Endpoints))
		for i, e := range fakeserver.Endpoints {
			names[i] = strings.TrimPrefix(string(e), "/")
		}

		fmt.Fprintf(fs.Output(), usageHeader, strings.Join(names, ", "))
		fs.PrintDefaults()
	}

	var logLevel string

	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:18099", "`host:port` to listen on; port 0 picks a free one, named in the listening line")
	fs.StringVar(&cfg.apiKey, "api-key", fakeserver.DefaultAPIKey, "the ApiKey header every request must carry, the agent's -k")
	fs.Func("fin-actions", "comma-separated `actions` for one /m3-fin reply, e.g. \"capture db\"; "+
		"repeat the flag for later calls, and the last list repeats", func(v string) error {
		cfg.finActions = append(cfg.finActions, splitList(v))

		return nil
	})
	fs.Func("custom-params", "the `urlParams` of a custom command the agent is configured with, e.g. tp=pidstat, "+
		"whose uploads carry no dt; repeat for each", func(v string) error {
		cfg.customParams = append(cfg.customParams, v)

		return nil
	})
	fs.StringVar(&cfg.secretsFile, "secrets-file", "", "a `file` of strings, one per line, that must never reach the server, "+
		"such as a test database password")
	fs.Func("status", "answer an endpoint with this status, as `endpoint=code`, e.g. yc-fin=500; comma-separated or repeated",
		func(v string) error {
			return eachPair(v, func(e fakeserver.Endpoint, value string) error {
				code, err := strconv.Atoi(value)
				if err != nil || code < 200 || code > 599 {
					return fmt.Errorf("%q is not a final HTTP status, 200 to 599", value)
				}

				reply := cfg.replies[e]
				reply.Status = code
				cfg.replies[e] = reply

				return nil
			})
		})
	fs.Func("delay", "wait before answering an endpoint, as `endpoint=duration`, e.g. yc-fin=5s; comma-separated or repeated",
		func(v string) error {
			return eachPair(v, func(e fakeserver.Endpoint, value string) error {
				d, err := time.ParseDuration(value)
				if err != nil || d < 0 {
					return fmt.Errorf("%q is not a duration", value)
				}

				reply := cfg.replies[e]
				reply.Delay = d
				cfg.replies[e] = reply

				return nil
			})
		})
	fs.Func("drop", "close the connection without answering an `endpoint`, e.g. yc-fin; comma-separated or repeated",
		func(v string) error {
			for _, name := range splitList(v) {
				e, ok := fakeserver.ParseEndpoint(name)
				if !ok {
					return fmt.Errorf("%q is not an endpoint", name)
				}

				reply := cfg.replies[e]
				reply.Drop = true
				cfg.replies[e] = reply
			}

			return nil
		})
	fs.BoolVar(&cfg.tls, "tls", false, "serve HTTPS; the CA that signed the certificate is written to <out>/ca.pem, "+
		"for the agent's caCertPath, which takes the absolute path the listening line names")
	fs.Func("tls-host", "another `name` or address the certificate covers besides localhost; comma-separated or repeated",
		func(v string) error {
			cfg.tlsHosts = append(cfg.tlsHosts, splitList(v)...)

			return nil
		})
	fs.StringVar(&cfg.out, "out", "yc-received", "the `directory` received files go to; requests.jsonl and each run's "+
		"requests.txt are appended to, and requests are numbered on from the last one there")
	fs.BoolVar(&cfg.noBodies, "no-bodies", false, "keep only each body's size and SHA-256, for a long run")
	fs.StringVar(&cfg.logFormat, "log-format", "text", "the log's `format` on stdout: text or json")
	fs.StringVar(&logLevel, "log-level", "info", "the least severe per-request `level` shown: debug, info, warn or error; "+
		"debug adds each request's query and headers. Startup and shutdown lines always show")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	// Absolute, because the agent reads caCertPath again after changing into yc-<ts>/.
	out, err := filepath.Abs(cfg.out)
	if err != nil {
		return cfg, fmt.Errorf("-out: %w", err)
	}

	cfg.out = out

	if cfg.logFormat != "text" && cfg.logFormat != "json" {
		return cfg, fmt.Errorf("-log-format %q: want text or json", cfg.logFormat)
	}

	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

	level, ok := levels[logLevel]
	if !ok {
		return cfg, fmt.Errorf("-log-level %q: want debug, info, warn or error", logLevel)
	}

	cfg.logLevel = level

	return cfg, nil
}

// eachPair calls f for every endpoint=value pair in a comma-separated list.
func eachPair(v string, f func(fakeserver.Endpoint, string) error) error {
	for _, pair := range splitList(v) {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("%q: want endpoint=value", pair)
		}

		e, ok := fakeserver.ParseEndpoint(name)
		if !ok {
			return fmt.Errorf("%q is not an endpoint", name)
		}

		if err := f(e, value); err != nil {
			return err
		}
	}

	return nil
}

func splitList(v string) []string {
	out := []string{}

	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}

	return out
}
