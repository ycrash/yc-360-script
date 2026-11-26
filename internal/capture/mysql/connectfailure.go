package mysql

import (
	"errors"
	"fmt"

	gomysql "github.com/go-sql-driver/mysql"

	"yc-agent/internal/capture/dbcore"
)

// configBlock is the target's block in the -c file.
const configBlock = "mysql"

// accountHost points at the account's host part.
const accountHost = ", and the account's host part ('user'@'host')"

// DescribeConnectFailure explains a refused Connect; a server's answer outranks a network error.
func DescribeConnectFailure(err error, t Target) dbcore.ConnectFailure {
	switch {
	case errors.Is(err, ErrTooManyConnections):
		return dbcore.ConnectFailure{
			Cause: "the server has no free connection slot (max_connections, or the account's max_user_connections)",
			Fix:   "Free a connection or raise the limit, then run the capture again",
		}

	case errors.Is(err, ErrPublicKeyRetrieval):
		return dbcore.ConnectFailure{
			Cause: "the login needs the server's RSA public key over a plaintext connection, which the agent " +
				"never requests; a wrong username or password ends here too",
			Fix: "Set tls: enabled: true in the mysql: block, then check username and password",
		}

	case errors.Is(err, gomysql.ErrNoTLS):
		return dbcore.ConnectFailure{
			Cause: "tls is enabled for this target, but the server does not accept encrypted connections",
			Fix:   "Enable TLS on the server, or set tls: enabled: false in the mysql: block",
		}
	}

	switch number := errorNumber(err); number {
	case 0:

	case errAccessDenied:
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("the server rejected the login of user %q: the username or the password is wrong, "+
				"or the account does not allow connections from this machine", t.Username),
			Fix: dbcore.CheckSettings(configBlock, "username", "password") + accountHost,
		}

	case errDBAccessDenied:
		// Also a missing database, for a user without global privileges.
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("database %q does not exist, or user %q has no access to it", t.Database, t.Username),
			Fix:   dbcore.CheckSettings(configBlock, "database") + ", or grant the user access to it",
		}

	case errUnknownDatabase:
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("database %q does not exist on the server", t.Database),
			Fix:   dbcore.CheckSettings(configBlock, "database"),
		}

	case errHostNotAllowed:
		return dbcore.ConnectFailure{
			Cause: "the server has no account that allows connections from this machine's address",
			Fix:   dbcore.CheckSettings(configBlock, "username") + accountHost,
		}

	case errSecureTransport:
		return dbcore.ConnectFailure{
			Cause: "the server accepts only encrypted connections (require_secure_transport)",
			Fix:   "Set tls: enabled: true in the mysql: block",
		}

	default:
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("the server refused the connection (error %d)", number),
			Fix:   "The server's reason is in the details below",
		}
	}

	if failure, ok := dbcore.NetworkFailure(err, configBlock, t.address(), ConnectTimeout); ok {
		return failure
	}

	return dbcore.ConnectFailure{
		Cause: "the connection to " + t.address() + " failed",
		Fix:   dbcore.CheckSettings(configBlock, "host", "port", "tls"),
	}
}
