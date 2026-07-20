//go:build !windows

package capture

import (
	"net"
	"time"
)

// dialNodePipe connects to the hook's Unix domain socket.
func dialNodePipe(pipePath string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", pipePath, timeout)
}
