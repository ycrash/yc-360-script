package postgres

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"yc-agent/internal/capture/dbcore"
)

// configBlock is the target's block in the -c file.
const configBlock = "postgres"

// Login refusals by SQLSTATE.
const (
	invalidPassword      = "28P01"
	invalidAuthorization = "28000" // a missing role under trust, NOLOGIN, or pg_hba.conf
	unknownDatabase      = "3D000"
	noConnectPrivilege   = "42501" // no CONNECT on the database
	cannotConnectNow     = "57P03" // starting up, shutting down, or recovering
)

// tlsRefusedText is pgx's error for a server that declines TLS.
const tlsRefusedText = "server refused TLS connection"

// DescribeConnectFailure explains a refused Connect; a server's answer outranks a
// refused address, since pgx tries each address a name resolves to.
func DescribeConnectFailure(err error, t Target) dbcore.ConnectFailure {
	address := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))

	if errors.Is(err, ErrTooManyConnections) {
		return dbcore.ConnectFailure{
			Cause: "the server has no free connection slot (max_connections, or the role's or the database's CONNECTION LIMIT)",
			Fix:   "Free a connection or raise the limit, then run the capture again",
		}
	}

	switch code := sqlState(err); code {
	case "":

	case invalidPassword:
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("the server rejected the login of user %q: the username or the password is wrong", t.Username),
			Fix:   dbcore.CheckSettings(configBlock, "username", "password"),
		}

	case invalidAuthorization:
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("the server refused user %q on database %q: the user does not exist, may not log in, "+
				"or pg_hba.conf does not admit it from this machine", t.Username, t.Database),
			Fix: dbcore.CheckSettings(configBlock, "username", "database") + ", and the server's pg_hba.conf",
		}

	case unknownDatabase:
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("database %q does not exist on the server", t.Database),
			Fix:   dbcore.CheckSettings(configBlock, "database"),
		}

	case noConnectPrivilege:
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("user %q may not connect to database %q (no CONNECT privilege)", t.Username, t.Database),
			Fix:   dbcore.CheckSettings(configBlock, "database") + ", or grant the user CONNECT on it",
		}

	case cannotConnectNow:
		return dbcore.ConnectFailure{
			Cause: "the server is starting up, shutting down or recovering, and is not accepting connections yet",
			Fix:   "Run the capture again once the database is up",
		}

	default:
		return dbcore.ConnectFailure{
			Cause: fmt.Sprintf("the server refused the connection (SQLSTATE %s)", code),
			Fix:   "The server's reason is in the details below",
		}
	}

	if err != nil && strings.Contains(err.Error(), tlsRefusedText) {
		return dbcore.ConnectFailure{
			Cause: "tls is enabled for this target, but the server does not accept encrypted connections",
			Fix:   "Enable SSL on the server, or set tls: enabled: false in the postgres: block",
		}
	}

	if failure, ok := dbcore.NetworkFailure(err, configBlock, address, ConnectTimeout); ok {
		return failure
	}

	return dbcore.ConnectFailure{
		Cause: "the connection to " + address + " failed",
		Fix:   dbcore.CheckSettings(configBlock, "host", "port", "tls"),
	}
}
