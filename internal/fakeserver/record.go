package fakeserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Request is what the fake recorded about one request. The API key and the
// secrets are masked everywhere except in the stored body.
type Request struct {
	Seq        int           `json:"seq"` // arrival order: from 1, or on from Dir's requests.jsonl
	Time       time.Time     `json:"time"`
	Method     string        `json:"method"`
	Path       string        `json:"path"`
	DT         string        `json:"dt"`
	Query      url.Values    `json:"query"`
	Header     http.Header   `json:"header"`
	Auth       string        `json:"auth"`   // AuthOK, AuthMissing or AuthMismatch
	Status     int           `json:"status"` // 0 when no reply was written
	Bytes      int64         `json:"bytes"`
	SHA256     string        `json:"sha256"`
	File       string        `json:"file"` // relative to Dir, with forward slashes; "" when no body was kept
	Duration   time.Duration `json:"duration"`
	Reply      string        `json:"reply"`
	Err        string        `json:"err"` // why the exchange did not complete
	Violations []Violation   `json:"violations"`

	dir      string
	run      string // the directory under dir for this request's files
	rawQuery string
}

// Body reads the stored body back. An empty body has no file and reads back empty.
func (r Request) Body() ([]byte, error) {
	if r.File == "" && r.Bytes == 0 {
		return []byte{}, nil
	}

	if r.File == "" {
		return nil, fmt.Errorf("request %d: no body was kept", r.Seq)
	}

	return os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(r.File)))
}

// Violation is one failed check.
type Violation struct {
	Seq    int    `json:"seq"`
	Rule   string `json:"rule"`
	Path   string `json:"path"`
	DT     string `json:"dt"`
	Detail string `json:"detail"`
}

func (v Violation) String() string {
	return fmt.Sprintf("request %d %s dt=%q: %s: %s", v.Seq, v.Path, v.DT, v.Rule, v.Detail)
}

func (r *Request) violate(rule, detail string) {
	r.Violations = append(r.Violations, Violation{Seq: r.Seq, Rule: rule, Path: r.Path, DT: r.DT, Detail: detail})
}

// replyLogLimit keeps a long reply body from taking over a log line.
const replyLogLimit = 200

// requestsTxt is each run directory's list of its requests, which no body may take.
const requestsTxt = "requests.txt"

// requestsJSONL holds every record, under Dir.
const requestsJSONL = "requests.jsonl"

// lastSeq is the highest seq in an existing requests.jsonl, so a fake started again
// on the same Dir numbers on from it; 0 when there is none. A line cut short when
// the last fake stopped is skipped.
func lastSeq(path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, err
	}

	defer f.Close()

	last := 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)

	for scanner.Scan() {
		var r struct {
			Seq int `json:"seq"`
		}

		if json.Unmarshal(scanner.Bytes(), &r) == nil && r.Seq > last {
			last = r.Seq
		}
	}

	return last, scanner.Err()
}

// record keeps a finished request and writes its lines to requests.txt,
// requests.jsonl and the log, under the lock so one request's lines stay together.
func (s *Server) record(req Request, storeErr, bodyErr error) {
	if req.Violations == nil {
		req.Violations = []Violation{}
	}

	now := time.Now()
	line := slog.NewRecord(now, slog.LevelInfo, "request", 0)
	line.AddAttrs(
		slog.String("method", req.Method),
		slog.String("path", req.Path),
		slog.String("dt", req.DT),
		slog.Int("status", req.Status),
		slog.Int64("bytes", req.Bytes),
		slog.Duration("duration", req.Duration.Round(10*time.Microsecond)),
		slog.String("auth", req.Auth),
		slog.String("file", req.File),
		slog.String("reply", truncate(req.Reply, replyLogLimit)),
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = append(s.requests, req)

	if err := s.appendTextLine(req.run, line); err != nil {
		s.storeFailed(now, req, requestsTxt, err)
	}

	if err := s.appendJSONLine(req); err != nil {
		s.storeFailed(now, req, "requests.jsonl", err)
	}

	s.emit(line)

	detail := slog.NewRecord(now, slog.LevelDebug, "request detail", 0)
	detail.AddAttrs(
		slog.String("path", req.Path),
		slog.String("query", s.masker.maskRawQuery(req.rawQuery)),
		slog.String("header", formatHeader(req.Header)),
	)
	s.emit(detail)

	for _, v := range req.Violations {
		warn := slog.NewRecord(now, slog.LevelWarn, "violation", 0)
		warn.AddAttrs(
			slog.String("rule", v.Rule),
			slog.String("path", v.Path),
			slog.String("dt", v.DT),
			slog.String("detail", v.Detail),
		)
		s.emit(warn)
	}

	if bodyErr != nil {
		cut := slog.NewRecord(now, slog.LevelError, "upload cut off", 0)
		cut.AddAttrs(
			slog.String("path", req.Path),
			slog.String("dt", req.DT),
			slog.Int64("bytes", req.Bytes),
			slog.String("err", bodyErr.Error()),
			slog.String("file", req.File),
		)
		s.emit(cut)
	}

	if storeErr != nil {
		s.storeFailed(now, req, "body", storeErr)
	}
}

// storeFailed reports a file the fake could not write. The agent is not at fault,
// so the reply is unchanged.
func (s *Server) storeFailed(now time.Time, req Request, what string, err error) {
	rec := slog.NewRecord(now, slog.LevelError, "store failed", 0)
	rec.AddAttrs(
		slog.String("path", req.Path),
		slog.String("dt", req.DT),
		slog.String("what", what),
		slog.String("err", err.Error()),
	)
	s.emit(rec)
}

func (s *Server) emit(rec slog.Record) {
	if s.opts.Log == nil {
		return
	}

	h := s.opts.Log.Handler()
	if h.Enabled(context.Background(), rec.Level) {
		_ = h.Handle(context.Background(), rec)
	}
}

// appendTextLine adds the request line to the run's requests.txt, in the text
// format whatever the log uses.
func (s *Server) appendTextLine(run string, line slog.Record) error {
	var buf bytes.Buffer
	if err := slog.NewTextHandler(&buf, nil).Handle(context.Background(), line); err != nil {
		return err
	}

	dir := filepath.Join(s.opts.Dir, run)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	return appendFile(filepath.Join(dir, requestsTxt), buf.Bytes())
}

func (s *Server) appendJSONLine(req Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}

	return appendFile(filepath.Join(s.opts.Dir, requestsJSONL), append(b, '\n'))
}

func appendFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}

	_, err = f.Write(b)

	return errors.Join(err, f.Close())
}

// formatHeader renders a header as one value: names sorted, one "name: values" each.
func formatHeader(h http.Header) string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}

	sort.Strings(names)

	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = name + ": " + strings.Join(h[name], ", ")
	}

	return strings.Join(parts, "; ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}
