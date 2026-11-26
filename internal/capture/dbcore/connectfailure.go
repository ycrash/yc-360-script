package dbcore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// ConnectFailure explains a refused dial on the console.
type ConnectFailure struct {
	// Cause is what went wrong.
	Cause string

	// Fix is what to check; may be empty.
	Fix string
}

// CheckSettings renders "Check <keys> in the <block>: block".
func CheckSettings(block string, keys ...string) string {
	return fmt.Sprintf("Check %s in the %s: block", joinWords(keys), block)
}

func joinWords(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}

	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// NetworkFailure explains a dial that failed before any server answered; ok is
// false otherwise.
func NetworkFailure(err error, block, address string, timeout time.Duration) (failure ConnectFailure, ok bool) {
	var (
		dnsErr  *net.DNSError
		certErr *tls.CertificateVerificationError
		opErr   *net.OpError
	)

	switch {
	case errors.As(err, &dnsErr):
		return ConnectFailure{
			Cause: fmt.Sprintf("the host name %q could not be resolved", dnsErr.Name),
			Fix:   CheckSettings(block, "host"),
		}, true

	case errors.Is(err, refusedErrno):
		return ConnectFailure{
			Cause: "nothing is accepting connections at " + address,
			Fix:   CheckSettings(block, "port", "host") + ", and that the database is running",
		}, true

	case errors.Is(err, context.Canceled):
		return ConnectFailure{Cause: "the run was stopped before the connection opened"}, true

	case timedOut(err):
		return ConnectFailure{
			Cause: fmt.Sprintf("%s did not answer within %s", address, timeout),
			Fix:   CheckSettings(block, "host", "port") + ", and that no firewall blocks the connection",
		}, true

	case errors.As(err, &certErr):
		return ConnectFailure{
			Cause: "the server's TLS certificate failed verification",
			Fix:   CheckSettings(block, "tls.caFile", "tls.serverName"),
		}, true

	case errors.As(err, &opErr):
		return ConnectFailure{
			Cause: address + " could not be reached",
			Fix:   CheckSettings(block, "host", "port"),
		}, true
	}

	return ConnectFailure{}, false
}

// timedOut covers the dial's deadline and a network timeout.
func timedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}
