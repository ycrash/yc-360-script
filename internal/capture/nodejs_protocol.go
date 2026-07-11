package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"yc-agent/internal/config"
)

const nodeRuntimeDirEnv = "YC360_NODE_RUNTIME_DIR"

// NodeRuntimeDir returns the directory where the hook writes its registration,
// token and socket files.
func NodeRuntimeDir() string {
	if configured := strings.TrimSpace(config.GlobalConfig.NodejsRuntimeDir); configured != "" {
		return configured
	}
	if env := strings.TrimSpace(os.Getenv(nodeRuntimeDirEnv)); env != "" {
		return env
	}
	return filepath.Join(os.TempDir(), "yc360", "node")
}

// NodeRegistration represents the JSON the hook writes to <runtimeDir>/<pid>.json
// on successful startup.
type NodeRegistration struct {
	PID         int    `json:"pid"`
	PipePath    string `json:"pipePath"`
	NodeVersion string `json:"nodeVersion"`
	Platform    string `json:"platform"`
	StartedAt   string `json:"startedAt"`
}

func nodeRegistrationPath(runtimeDir string, pid int) string {
	return filepath.Join(runtimeDir, strconv.Itoa(pid)+".json")
}

// ReadNodeRegistration reads and parses the hook registration file for pid.
func ReadNodeRegistration(runtimeDir string, pid int) (*NodeRegistration, error) {
	path := nodeRegistrationPath(runtimeDir, pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var reg NodeRegistration
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("registration file %s is not valid JSON: %w", path, err)
	}
	if reg.PID != pid {
		return nil, fmt.Errorf("registration file %s pid=%d does not match target pid=%d (stale file / PID reuse)", path, reg.PID, pid)
	}
	if strings.TrimSpace(reg.PipePath) == "" {
		return nil, fmt.Errorf("registration file %s has no pipePath", path)
	}
	return &reg, nil
}

const nodeTokenFileName = "token"

var nodeTokenCache struct {
	sync.Mutex
	dir    string
	token  string
	loaded bool
}

// NodeToken returns the shared-secret token from <runtimeDir>/token, cached
// after the first successful read.
func NodeToken(runtimeDir string) (string, error) {
	nodeTokenCache.Lock()
	defer nodeTokenCache.Unlock()

	if nodeTokenCache.loaded && nodeTokenCache.dir == runtimeDir {
		return nodeTokenCache.token, nil
	}

	path := filepath.Join(runtimeDir, nodeTokenFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed reading node hook token %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("node hook token %s is empty", path)
	}

	nodeTokenCache.dir = runtimeDir
	nodeTokenCache.token = token
	nodeTokenCache.loaded = true
	return token, nil
}

// InvalidateNodeToken clears the cached token, forcing the next NodeToken call
// to re-read from disk.
func InvalidateNodeToken() {
	nodeTokenCache.Lock()
	nodeTokenCache.loaded = false
	nodeTokenCache.token = ""
	nodeTokenCache.Unlock()
}

// nodeRequest is a single RPC request line (newline-delimited JSON).
type nodeRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
	Token  string `json:"token"`
}

type nodeResponse struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
	Result json.RawMessage `json:"result"`
}

// errNodeUnauthorized is returned when the hook rejects a request's token.
var errNodeUnauthorized = fmt.Errorf("node hook rejected request: unauthorized")

// IsWindows reports whether the agent is running on Windows
func IsWindows() bool { return runtime.GOOS == "windows" }
