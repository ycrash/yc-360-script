package capture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"yc-agent/internal/logger"
)

// maxNodeResponseBytes caps how much the agent will buffer while reading a
// single hook response, mirroring the hook's own MAX_REQUEST_BUFFER_BYTES
// (1 MiB). Without this, a buggy or hostile peer that never sends a newline
// could drive the agent to memory exhaustion within the read deadline.
const maxNodeResponseBytes = 1 << 20

// nodeReqCounter yields unique, monotonically increasing request ids within a
// single agent run.
var nodeReqCounter atomic.Int64

// NodeHookClient talks to a single Node.js process's hook over its local IPC
// endpoint (Unix domain socket on POSIX, named pipe on Windows).
type NodeHookClient struct {
	RuntimeDir string
	PID        int
	Reg        *NodeRegistration
}

// NewNodeHookClient discovers the hook registration for pid under runtimeDir
// and returns a client.
func NewNodeHookClient(runtimeDir string, pid int) (*NodeHookClient, error) {
	reg, err := ReadNodeRegistration(runtimeDir, pid)
	if err != nil {
		return nil, err
	}
	return &NodeHookClient{RuntimeDir: runtimeDir, PID: pid, Reg: reg}, nil
}

// PipePath returns the IPC endpoint path from the registration file.
func (c *NodeHookClient) PipePath() string { return c.Reg.PipePath }

// call performs one RPC: it reads (or reuses) the cached token, sends the
// request, and returns the decoded response.
func (c *NodeHookClient) call(method string, params any, timeout time.Duration) (*nodeResponse, error) {
	resp, err := c.callOnce(method, params, timeout)
	if err == errNodeUnauthorized {
		// Token may have rotated — force a fresh read and retry exactly once.
		InvalidateNodeToken()
		resp, err = c.callOnce(method, params, timeout)
	}
	return resp, err
}

func (c *NodeHookClient) callOnce(method string, params any, timeout time.Duration) (*nodeResponse, error) {
	token, err := NodeToken(c.RuntimeDir)
	if err != nil {
		return nil, err
	}

	conn, err := dialNodePipe(c.Reg.PipePath, nodeDialTimeout(timeout))
	if err != nil {
		return nil, fmt.Errorf("failed connecting to node hook pid=%d at %s: %w", c.PID, c.Reg.PipePath, err)
	}
	defer conn.Close()

	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	reqID := nodeRequestID(method)
	req := nodeRequest{ID: reqID, Method: method, Params: params, Token: token}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed encoding node %s request: %w", method, err)
	}
	line = append(line, '\n')
	if _, err := conn.Write(line); err != nil {
		return nil, fmt.Errorf("failed sending node %s request pid=%d: %w", method, c.PID, err)
	}

	// Read exactly one newline-terminated response line. The hook writes one
	// response object per request; we match on id to avoid trusting ordering.
	reader := bufio.NewReader(io.LimitReader(conn, maxNodeResponseBytes+1))
	for {
		respLine, err := reader.ReadBytes('\n')
		if int64(len(respLine)) > maxNodeResponseBytes {
			return nil, fmt.Errorf("node %s response pid=%d exceeded the %d-byte cap without newline framing", method, c.PID, maxNodeResponseBytes)
		}
		if err != nil {
			return nil, fmt.Errorf("failed reading node %s response pid=%d: %w", method, c.PID, err)
		}
		if len(respLine) == 0 {
			continue
		}

		var resp nodeResponse
		if err := json.Unmarshal(respLine, &resp); err != nil {
			return nil, fmt.Errorf("node %s response is not valid JSON pid=%d: %w", method, c.PID, err)
		}

		// A framing error / auth failure can carry id=null; accept it. Otherwise
		// require the id to match the request we sent.
		if resp.ID != "" && resp.ID != reqID {
			logger.Debug().Str("method", method).Str("want", reqID).Str("got", resp.ID).Msg("node hook response id mismatch; ignoring line")
			continue
		}

		if !resp.OK && resp.Error == "unauthorized" {
			return &resp, errNodeUnauthorized
		}
		return &resp, nil
	}
}

// nodeDialTimeout derives a bounded connect timeout from the overall request
// timeout.
func nodeDialTimeout(overall time.Duration) time.Duration {
	const maxDial = 10 * time.Second
	if overall <= 0 || overall > maxDial {
		return maxDial
	}
	return overall
}

// nodeRequestID generates a unique-per-request id.
func nodeRequestID(method string) string {
	n := nodeReqCounter.Add(1)
	return fmt.Sprintf("yc-%s-%d", method, n)
}

// dialNodePipe is implemented per-platform (nodejs_ipc_dial_unix.go /
// nodejs_ipc_dial_windows.go): net.Dial("unix", ...) on POSIX, a named-pipe
// open on Windows.
