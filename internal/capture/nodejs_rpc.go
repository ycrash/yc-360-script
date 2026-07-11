package capture

import (
	"encoding/json"
	"fmt"
	"time"
)

// This file wraps the hook RPCs as typed Go methods on NodeHookClient. The wire
// details (framing, token auth, id-matched request/response) live in
// nodejs_ipc.go; here we deal with params, result decoding and per-RPC timeouts.
//
// Item #3 (hook IPC client) wires only the liveness/identity RPC, ping — the one
// the discovery handshake needs to confirm a hook socket is actually responsive.
// The capture RPCs (dumpProcessOverview, dumpHeapSummary, dumpGC, dumpCPUProfile
// and the Diagnostic Report page captures) arrive with item #4 and append here;
// the asynchronous ones derive their own longer timeouts from the requested
// window so the read deadline outlasts the hook's delayed response.

const (
	// nodeSyncTimeout bounds the synchronous RPCs (currently just ping). These
	// respond near-instantly, so the timeout is generous.
	nodeSyncTimeout = 60 * time.Second
)

// NodePingResult is the identity/liveness payload returned by ping.
type NodePingResult struct {
	PID         int     `json:"pid"`
	NodeVersion string  `json:"nodeVersion"`
	Platform    string  `json:"platform"`
	UptimeSec   float64 `json:"uptimeSec"`
	HookVersion string  `json:"hookVersion"`
}

// Ping confirms the hook is responsive and returns its identity. It is called
// during discovery (ResolveNodeCapture) to distinguish a live hook from a stale
// registration file left behind by a crashed process or PID reuse.
func (c *NodeHookClient) Ping() (*NodePingResult, error) {
	resp, err := c.call("ping", nil, nodeSyncTimeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node ping failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodePingResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node ping result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}
