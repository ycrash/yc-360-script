package fakeserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Rules a request can break.
const (
	RulePath        = "path"
	RuleMethod      = "method"
	RuleAuth        = "auth"
	RuleContentType = "content-type"
	RuleQuery       = "query"
	RuleKeyLeak     = "key-leak"
	RuleSecretLeak  = "secret-leak"
)

// Values of Request.Auth.
const (
	AuthOK       = "ok"
	AuthMissing  = "missing"
	AuthMismatch = "mismatch"
)

// sniffLen is how much of a body http.DetectContentType reads.
const sniffLen = 512

// serve checks and records one request, then answers as its Reply says. The
// record comes first, so a client that has its answer finds it in Requests.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	query := r.URL.Query()
	req := Request{
		Seq:    seq,
		Time:   start,
		Method: r.Method,
		Path:   s.masker.mask(r.URL.Path),
		DT:     s.masker.mask(query.Get("dt")),
		Query:  s.masker.maskValues(query),
		Header: s.masker.maskHeader(r.Header),

		dir:      s.opts.Dir,
		run:      s.runDir(query),
		rawQuery: r.URL.RawQuery,
	}

	auth, authDetail := s.checkAuth(r.Header)
	req.Auth = auth

	shape, known := endpointShapes[Endpoint(r.URL.Path)]
	if known {
		s.checkRequest(&req, r, shape, query, authDetail)
	} else {
		req.violate(RulePath, "not an endpoint the agent requests")
	}

	s.checkLeaks(&req, r, query)

	found, storeErr, bodyErr := s.receive(&req, r.Body, query)
	for _, i := range found {
		req.violate(RuleSecretLeak, fmt.Sprintf("secret %d is in the body", i+1))
	}

	// Status 0 means no reply was written, so these close the connection rather
	// than leave net/http to send its own 200.
	if bodyErr != nil {
		req.Err = "upload cut off: " + bodyErr.Error()
		req.Duration = time.Since(start)
		s.record(req, storeErr, bodyErr)
		dropConnection(w)

		return
	}

	status, body, delay, drop := s.answer(r, Endpoint(r.URL.Path), known, auth)

	if delay > 0 && !wait(r.Context(), delay) {
		req.Err = "the connection closed before the reply"
		req.Duration = time.Since(start)
		s.record(req, storeErr, nil)
		dropConnection(w)

		return
	}

	if !bodyAllowed(r.Method, status) {
		body = ""
	}

	if drop {
		req.Err = "dropped without a reply"
	} else {
		req.Status = status
		req.Reply = body
	}

	req.Duration = time.Since(start)
	s.record(req, storeErr, nil)

	if drop {
		dropConnection(w)

		return
	}

	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// bodyAllowed reports whether a reply can carry a body: not one to a HEAD
// request, and not a 204 or 304.
func bodyAllowed(method string, status int) bool {
	return method != http.MethodHead && status != http.StatusNoContent && status != http.StatusNotModified
}

// checkAuth compares the ApiKey header with the expected key.
func (s *Server) checkAuth(h http.Header) (auth, detail string) {
	values := h.Values("ApiKey")

	switch {
	case len(values) == 0:
		return AuthMissing, "no ApiKey header"
	case values[0] == "":
		return AuthMissing, "the ApiKey header is empty"
	case values[0] != s.key:
		return AuthMismatch, "the ApiKey header does not match"
	}

	return AuthOK, ""
}

// checkRequest holds a request to one of the endpoints to what the agent sends there.
func (s *Server) checkRequest(req *Request, r *http.Request, shape endpointShape, q url.Values, authDetail string) {
	if r.Method != shape.method {
		req.violate(RuleMethod, fmt.Sprintf("%s, want %s", r.Method, shape.method))
	}

	if r.Method == http.MethodPost {
		if ct := r.Header.Get("Content-Type"); ct != "text" {
			req.violate(RuleContentType, fmt.Sprintf("Content-Type %q, want \"text\"", s.masker.mask(ct)))
		}
	}

	if req.Auth != AuthOK {
		req.violate(RuleAuth, authDetail)
	}

	var missing []string

	for _, p := range shape.params {
		if q.Get(p) == "" {
			missing = append(missing, describeMissing(p, q))
		}
	}

	if shape.upload && q.Get("dt") == "" && !s.isCustom(q) {
		missing = append(missing, "dt")
	}

	if len(missing) > 0 {
		req.violate(RuleQuery, "missing "+strings.Join(missing, ", "))
	}
}

// describeMissing names a missing parameter, and the spelling it came in if the
// only difference is case.
func describeMissing(param string, q url.Values) string {
	for k := range q {
		if k != param && strings.EqualFold(k, param) {
			return fmt.Sprintf("%s (sent as %s)", param, k)
		}
	}

	return param
}

// isCustom reports whether q carries every parameter of one custom command.
func (s *Server) isCustom(q url.Values) bool {
	for _, params := range s.custom {
		if len(params) > 0 && hasAll(q, params) {
			return true
		}
	}

	return false
}

func hasAll(q, want url.Values) bool {
	for k, values := range want {
		for _, v := range values {
			if !slices.Contains(q[k], v) {
				return false
			}
		}
	}

	return true
}

// checkLeaks looks for the key in the URL, and for the secrets in the URL and the headers.
func (s *Server) checkLeaks(req *Request, r *http.Request, q url.Values) {
	texts := []string{r.RequestURI, r.URL.Path}
	for k, values := range q {
		texts = append(texts, k)
		texts = append(texts, values...)
	}

	if s.masker.keyIn(texts) {
		req.violate(RuleKeyLeak, "the API key is in the URL")
	}

	for _, i := range s.masker.secretsIn(texts) {
		req.violate(RuleSecretLeak, fmt.Sprintf("secret %d is in the URL", i+1))
	}

	names := make([]string, 0, len(r.Header))
	for name := range r.Header {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		for _, i := range s.masker.secretsIn(append([]string{name}, r.Header[name]...)) {
			req.violate(RuleSecretLeak, fmt.Sprintf("secret %d is in the %s header", i+1, s.masker.mask(name)))
		}
	}
}

// receive streams the body to its file, hashing and scanning it on the way. A
// body that stops arriving keeps the part that came.
func (s *Server) receive(req *Request, body io.Reader, q url.Values) (found []int, storeErr, bodyErr error) {
	hash := sha256.New()
	scan := s.masker.scanner()

	head, ended, bodyErr := readHead(body, sniffLen)

	var file *os.File
	if len(head) > 0 && !s.opts.NoBodies {
		file, req.File, storeErr = s.create(req.run, q, head)
	}

	sinks := []io.Writer{hash, scan}
	if file != nil {
		sinks = append(sinks, &keepFirstError{w: file, err: &storeErr})
	}

	sink := io.MultiWriter(sinks...)
	_, _ = sink.Write(head)
	req.Bytes = int64(len(head))

	if !ended {
		n, err := io.Copy(sink, body)
		req.Bytes += n
		bodyErr = err
	}

	if file != nil {
		if err := file.Close(); err != nil && storeErr == nil {
			storeErr = err
		}

		// A body only partly on disk is not kept, so Body never returns it as whole.
		if storeErr != nil {
			_ = os.Remove(filepath.Join(s.opts.Dir, filepath.FromSlash(req.File)))
			req.File = ""
		}
	}

	req.SHA256 = hex.EncodeToString(hash.Sum(nil))

	return scan.foundSecrets(), storeErr, bodyErr
}

// readHead reads up to n bytes. ended reports that the body is over: it ended
// cleanly, or err says why it stopped.
func readHead(r io.Reader, n int) (head []byte, ended bool, err error) {
	buf := make([]byte, n)
	got := 0

	for got < n {
		m, err := r.Read(buf[got:])
		got += m

		if errors.Is(err, io.EOF) {
			return buf[:got], true, nil
		}

		if err != nil {
			return buf[:got], true, err
		}
	}

	return buf, false, nil
}

// keepFirstError lets the body go on being hashed and scanned after its file
// fails, and remembers why the file failed.
type keepFirstError struct {
	w   io.Writer
	err *error
}

func (k *keepFirstError) Write(p []byte) (int, error) {
	if *k.err == nil {
		if _, err := k.w.Write(p); err != nil {
			*k.err = err
		}
	}

	return len(p), nil
}

// create opens a new file for a body, named from the request alone: dt, pid, then
// the Content-Encoding parameter or what the first bytes look like.
func (s *Server) create(run string, q url.Values, head []byte) (*os.File, string, error) {
	base := "no-dt"
	if dt := q.Get("dt"); dt != "" {
		base = safeName(s.masker.mask(dt))
	}

	if pid := q.Get("pid"); pid != "" {
		base += ".pid-" + safeName(s.masker.mask(pid))
	}

	ext := extension(s.masker.mask(q.Get("Content-Encoding")), head)

	dir := filepath.Join(s.opts.Dir, run)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := run + "/" + base + "." + ext

	for n := s.names[key] + 1; ; n++ {
		name := base
		if n > 1 {
			name += "-" + strconv.Itoa(n)
		}

		name += "." + ext
		// On a file system that ignores case, Requests.txt is requests.txt too.
		if strings.EqualFold(name, requestsTxt) {
			continue
		}

		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}

		if err != nil {
			return nil, "", err
		}

		s.names[key] = n

		return f, run + "/" + name, nil
	}
}

// runDir is the directory a request's files and requests.txt line go in.
func (s *Server) runDir(q url.Values) string {
	if ts := q.Get("ts"); ts != "" {
		return "yc-" + safeName(s.masker.mask(ts))
	}

	return "no-ts"
}

// extension is the Content-Encoding parameter when there is one, otherwise txt
// for text and bin for anything else.
func extension(encoding string, head []byte) string {
	if ext := safeExtension(encoding); ext != "" {
		return ext
	}

	if strings.HasPrefix(http.DetectContentType(head), "text/") {
		return "txt"
	}

	return "bin"
}

func safeExtension(s string) string {
	var b strings.Builder

	for _, c := range []byte(s) {
		if isAlnum(c) && b.Len() < 16 {
			b.WriteByte(c)
		}
	}

	return b.String()
}

// safeName keeps a value from the request usable as one file name: no separators,
// no leading dot, nothing longer than a file system takes.
func safeName(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !isAlnum(c) && c != '-' && c != '_' && c != '.' {
			b[i] = '_'
		}
	}

	name := strings.Trim(string(b), ".")
	if len(name) > 100 {
		name = name[:100]
	}

	if name == "" {
		return "_"
	}

	return name
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// answer chooses the reply. Only a refused key changes it from what the test set.
func (s *Server) answer(r *http.Request, e Endpoint, known bool, auth string) (status int, body string, delay time.Duration, drop bool) {
	if !known {
		return http.StatusNotFound, "", 0, false
	}

	reply := s.opts.Replies[e]

	if auth != AuthOK {
		status = reply.AuthStatus
		if status == 0 {
			status = http.StatusUnauthorized
		}

		return status, "", 0, false
	}

	status = reply.Status
	if status == 0 {
		status = http.StatusOK
	}

	if reply.Body != nil {
		body = *reply.Body
	} else {
		body = s.defaultBody(r, e)
	}

	return status, body, reply.Delay, reply.Drop
}

// defaultBody lets a run with no options finish cleanly: /yc-fin names a report,
// so the agent prints its result table, and /m3-fin asks for the next actions.
func (s *Server) defaultBody(r *http.Request, e Endpoint) string {
	switch e {
	case Fin:
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}

		b, _ := json.Marshal(map[string]string{"dashboardReportURL": scheme + "://" + r.Host + "/report"})

		return string(b)
	case M3Fin:
		return s.nextFinActions()
	default:
		return "OK"
	}
}

// nextFinActions answers one /m3-fin call with the next list of actions.
func (s *Server) nextFinActions() string {
	s.mu.Lock()
	call := s.finCalls
	s.finCalls++
	s.mu.Unlock()

	actions := []string{}
	if n := len(s.opts.FinActions); n > 0 {
		actions = append(actions, s.opts.FinActions[min(call, n-1)]...)
	}

	b, _ := json.Marshal(struct {
		Actions []string `json:"actions"`
	}{actions})

	return string(b)
}

// wait sleeps for d, or until the client gives up; it reports whether d passed.
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// dropConnection closes the connection without writing a reply.
func dropConnection(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}

	_ = conn.Close()
}
