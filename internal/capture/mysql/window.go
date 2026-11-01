package mysql

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// engineName is engine= on every block header.
const engineName = "mysql"

const (
	ConnectionFast      = dbcore.ConnectionFast
	ConnectionNormal    = dbcore.ConnectionNormal
	ConnectionExpensive = dbcore.ConnectionExpensive
)

// Status values for an artifact's closing block; see dbcore for each one's meaning.
const (
	StatusComplete         = dbcore.StatusComplete
	StatusPartial          = dbcore.StatusPartial
	StatusCancelled        = dbcore.StatusCancelled
	StatusDeadlineExceeded = dbcore.StatusDeadlineExceeded
	StatusConnectionLost   = dbcore.StatusConnectionLost
	StatusConnectFailed    = dbcore.StatusConnectFailed
)

type (
	Schedule       = dbcore.Schedule
	Artifact       = dbcore.Artifact
	ArtifactResult = dbcore.ArtifactResult

	// The core's types, over this package's Querier and Facts.
	Collector     = dbcore.Collector[Querier, Facts]
	SampleContext = dbcore.SampleContext[Facts]
	Window        = dbcore.Window[Querier, Facts]
)

// Variants of the MySQL family, as my_metadata.txt names them.
const (
	VariantMySQL   = "mysql"
	VariantPercona = "percona"
	VariantMariaDB = "mariadb"
)

// Facts is what identify reads once per connection; every version gate uses it.
type Facts struct {
	Version Version
	Variant string

	// PerformanceSchemaOff: started with performance_schema=OFF; its tables then
	// return no rows (measured on 8.4.13). Unread reads as on.
	PerformanceSchemaOff bool
}

// Version is VERSION()'s numbers; zero if unread or unparsed.
type Version struct {
	Major, Minor, Patch int
}

// AtLeast reports v >= major.minor.patch. An unread version is never at least
// anything, so gates fall back to the older form.
func (v Version) AtLeast(major, minor, patch int) bool {
	switch {
	case v.Major != major:
		return v.Major > major
	case v.Minor != minor:
		return v.Minor > minor
	}

	return v.Patch >= patch
}

func (v Version) known() bool { return v.Major > 0 }

// parseVersion reads the leading x.y.z of VERSION(), e.g. "8.4.3-3",
// "10.11.6-MariaDB-1:10.11.6+maria~ubu2204".
func parseVersion(s string) Version {
	numbers := strings.SplitN(strings.SplitN(s, "-", 2)[0], ".", 3)
	if len(numbers) < 2 {
		return Version{}
	}

	var parts [3]int

	for i, n := range numbers {
		digits := n
		if end := strings.IndexFunc(n, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
			digits = n[:end]
		}

		value, err := strconv.Atoi(digits)
		if err != nil {
			return Version{}
		}

		parts[i] = value
	}

	return Version{Major: parts[0], Minor: parts[1], Patch: parts[2]}
}

// variantOf names the family member from VERSION() and version_comment.
func variantOf(version, versionComment string) string {
	switch {
	case strings.Contains(strings.ToLower(version), "mariadb"):
		return VariantMariaDB

	case strings.Contains(strings.ToLower(versionComment), "percona"):
		return VariantPercona
	}

	return VariantMySQL
}

const identifySQL = `SELECT DATABASE(), VERSION(), @@version_comment, @@performance_schema`

// NewWindow is the window for t's collectors, dialling with Connect.
func NewWindow(t Target, duration time.Duration, collectors []Collector, captureID string) *Window {
	return &Window{
		Engine:     engine(t, nil),
		Duration:   duration,
		Collectors: collectors,
		CaptureID:  captureID,
		TargetID:   t.id(),
		Database:   t.Database,
	}
}

// engine is the window's MySQL specifics; tests pass dial in place of Connect.
func engine(t Target, dial func(ctx context.Context) (Querier, dbcore.Closer, error)) dbcore.Engine[Querier, Facts] {
	if dial == nil {
		dial = func(ctx context.Context) (Querier, dbcore.Closer, error) {
			conn, err := Connect(ctx, t)
			if err != nil {
				return nil, nil, err
			}

			return conn, conn, nil
		}
	}

	return dbcore.Engine[Querier, Facts]{
		Name:     engineName,
		Dial:     dial,
		Identify: identify,
		Location: location,

		ErrorText:        func(err error) string { return errorText(err, t.Password) },
		ConnectErrorText: func(err error) string { return ConnectErrorText(err, t) },
		SlotRefused:      func(err error) bool { return errors.Is(err, ErrTooManyConnections) },
		ReadStatus:       readStatus,

		ConnectTimeout:      ConnectTimeout,
		DefaultSampleBudget: DefaultSampleBudget,
		CloseMargin:         WindowCloseMargin,

		// my_metadata.txt is read once, at the start.
		KeepsFinishedOnLoss: true,
	}
}

// identify fills SampleContext once per connection.
func identify(ctx context.Context, q Querier, sampleCtx SampleContext) SampleContext {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, identifySQL)
	if err != nil {
		return sampleCtx
	}
	defer rows.Close()

	if !rows.Next() {
		return sampleCtx
	}

	var database, version, comment, performanceSchema sql.NullString
	if err := rows.Scan(&database, &version, &comment, &performanceSchema); err != nil {
		return sampleCtx
	}

	sampleCtx.Database = database.String
	sampleCtx.EngineVersion = version.String
	sampleCtx.Engine.Version = parseVersion(version.String)
	sampleCtx.Engine.Variant = variantOf(version.String, comment.String)
	sampleCtx.Engine.PerformanceSchemaOff = performanceSchema.String == "0"

	return sampleCtx
}

// location is db=, the connection's default schema (MySQL statistics are
// instance-wide regardless).
func location(s SampleContext) []dbcore.HeaderField {
	return []dbcore.HeaderField{{Key: "db", Value: s.Database}}
}
