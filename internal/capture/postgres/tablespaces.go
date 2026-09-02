package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// Tablespace is one tablespace with storage of its own. pg_default and pg_global
// live inside the data directory and report an empty location, so they are not
// listed: the list is where the server looks for a volume df.out must cover.
type Tablespace struct {
	Name     string
	Location string
}

// tablespaceSQL lists the tablespaces outside the data directory, with their
// locations. Read every M3 poll and once per deep dive: a tablespace can be
// created while the server runs, and the list is tiny. pg_tablespace_location
// needs no grant - the matrix shows it for a LOGIN-only role.
const tablespaceSQL = `SELECT spcname::text, pg_tablespace_location(oid)
FROM pg_catalog.pg_tablespace
WHERE pg_tablespace_location(oid) <> ''
ORDER BY spcname`

// readTablespaces is shared by the M3 poll's disk reading and pg_metadata.txt's
// location block, so the two can never disagree about which volumes are the
// database's.
func readTablespaces(ctx context.Context, q RowQuerier) ([]Tablespace, error) {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(ctx, tablespaceSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tablespaces []Tablespace

	for rows.Next() {
		var (
			name     string
			location *string
		)

		if err := rows.Scan(&name, &location); err != nil {
			return nil, err
		}

		if location != nil && *location != "" {
			tablespaces = append(tablespaces, Tablespace{Name: name, Location: *location})
		}
	}

	return tablespaces, rows.Err()
}

// tablespaceColumns is the spec's pair. spcname is the key across samples - it
// is unique in pg_tablespace - and pg_reported_size_bytes is what the server
// sets against df.out's mounts, through the locations pg_metadata.txt carries.
var tablespaceColumns = []string{
	"spcname",
	"pg_reported_size_bytes",
}

// tablespaceSizesSQL guards the size function in the select list. It raises on
// a tablespace the role may not read, and an error in a select list aborts the
// whole statement: SQL has no per-row skip, and bloat's LEFT JOIN cannot express
// a privilege. The guard is the server's own rule (dbsize.c): the database's
// default tablespace, pg_read_all_stats, or CREATE on the tablespace. USAGE
// rather than MEMBER, because the server checks has_privs_of_role. An
// unreadable tablespace is then an empty cell, counted on the header.
const tablespaceSizesSQL = `SELECT spcname::text,
       CASE WHEN oid = (SELECT dattablespace FROM pg_catalog.pg_database WHERE datname = current_database())
              OR pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')
              OR has_tablespace_privilege(oid, 'CREATE')
            THEN pg_tablespace_size(oid)
       END AS pg_reported_size_bytes
FROM pg_catalog.pg_tablespace
ORDER BY spcname`

// Tablespaces captures every tablespace's size every sample; growth is computed
// downstream by joining consecutive blocks on spcname, and the size is set
// against the volume behind it, which pg_metadata.txt names and df.out measures
// where the agent runs on the database's machine.
type Tablespaces struct {
	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration
}

func (ts Tablespaces) Artifact() Artifact {
	return Artifact{
		Name:     "pg_tablespaces",
		FileName: "pg_tablespaces.txt",
		Scope:    "cluster",
		Schedule: Periodic(ts.Interval),

		// One statement, not DefaultSampleBudget's two: Periodic's last sample is
		// the close, and the shared tick is sized from this.
		SampleBudget: StatementTimeout,
	}
}

// Sample runs the one statement and writes one block. A statement that fails
// errors and writes nothing; a tablespace the role may not read is an empty
// cell in a written block.
func (ts Tablespaces) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, err := readTablespaceSizes(ctx, q)
	if err != nil {
		return err
	}

	unread := 0

	for _, row := range rows {
		if row.size == nil {
			unread++
		}
	}

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
		{"tablespaces", strconv.Itoa(len(rows))},
		{"sizes_unread", strconv.Itoa(unread)},
	}

	// Buffered so a write failure never leaves a half-written body.
	var block bytes.Buffer

	// Named for the function read; the window's own blocks name the artifact.
	if err := writeBlockHeader(&block, "pg_tablespace_size", ts.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	if err := writeRows(&block, tablespaceColumns, tablespaceCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// tablespaceSize is one row. size is a pointer for the package's rule: empty
// means the role may not read it, where 0 would claim an empty tablespace.
type tablespaceSize struct {
	name string
	size *int64
}

func readTablespaceSizes(ctx context.Context, q RowQuerier) ([]tablespaceSize, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(stmtCtx, tablespaceSizesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var collected []tablespaceSize

	for rows.Next() {
		var row tablespaceSize

		if err := rows.Scan(&row.name, &row.size); err != nil {
			return nil, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return collected, nil
}

func tablespaceCells(rows []tablespaceSize) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{row.name, int64Text(row.size)}
	}

	return cells
}
