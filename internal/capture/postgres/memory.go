package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// insufficientPrivilegeCode is the SQLSTATE of the server refusing a read to
// this role.
const insufficientPrivilegeCode = "42501"

// reasonPermissionDenied: the role may not read the view. pg_shmem_allocations
// needs pg_read_all_stats from PostgreSQL 15 (which pg_monitor includes) and
// superuser before that.
const reasonPermissionDenied = "permission_denied"

// readPrivilege runs one has_*_privilege check, asked before a read the role may
// be refused: the refusal itself would be an ERROR and its STATEMENT in the
// server's own log. NULL tries the read.
func readPrivilege(ctx context.Context, q Querier, sql string) (bool, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	var allowed *bool

	if err := q.QueryRow(stmtCtx, sql).Scan(&allowed); err != nil {
		return false, err
	}

	return allowed == nil || *allowed, nil
}

// memoryColumns is the view's four columns. name is the key across samples;
// the one row with no name is the shared memory not yet allocated.
var memoryColumns = []string{
	"name",
	"off",
	"size",
	"allocated_size",
}

// memoryPrivilegeSQL is asked before memorySQL on every sample.
const memoryPrivilegeSQL = `SELECT has_table_privilege('pg_catalog.pg_shmem_allocations', 'SELECT')`

// memorySQL is every named allocation in the server's shared memory, largest
// first, uncapped: the view has well under a hundred rows.
const memorySQL = `SELECT name, off, size, allocated_size
FROM pg_catalog.pg_shmem_allocations
ORDER BY allocated_size DESC`

// Memory captures the server's shared memory allocations every sample. Which
// allocation is the buffer pool, and which the engine's own, is the server's
// reading.
type Memory struct {
	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration
}

func (m Memory) Artifact() Artifact {
	return Artifact{
		Name:     "pg_memory",
		FileName: "pg_memory.txt",
		Scope:    "cluster",
		Schedule: Periodic(m.Interval),

		// No SampleBudget: the privilege check and the read are DefaultSampleBudget's
		// two statements.
	}
}

// Sample writes one block. A refusal is a written sample, reason=permission_denied
// with no rows, since it recurs every sample for this role; any other failure
// errors and writes nothing, and the window writes the stub.
func (m Memory) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, denied, err := readMemory(ctx, q)
	if err != nil {
		return err
	}

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	if denied {
		fields = append(fields, headerField{"reason", reasonPermissionDenied})
	}

	// Buffered so a write failure never leaves a half-written body.
	var block bytes.Buffer

	// Named for the view read; the window's own blocks name the artifact.
	if err := writeBlockHeader(&block, "pg_shmem_allocations", m.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	if err := writeRows(&block, memoryColumns, memoryCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// memoryRow is one allocation. name is NULL for the unallocated remainder, and
// off for the anonymous allocations, which have no one offset.
type memoryRow struct {
	name          *string
	off           *int64
	size          *int64
	allocatedSize *int64
}

// readMemory reports a refusal as denied, not an error: found by the privilege
// check, or by SQLSTATE 42501 when the grant goes between the check and the read.
func readMemory(ctx context.Context, q RowQuerier) ([]memoryRow, bool, error) {
	allowed, err := readPrivilege(ctx, q, memoryPrivilegeSQL)
	if err != nil {
		return nil, false, err
	}

	if !allowed {
		return nil, true, nil
	}

	rows, err := queryMemory(ctx, q)
	if hasSQLState(err, insufficientPrivilegeCode) {
		return nil, true, nil
	}

	return rows, false, err
}

func queryMemory(ctx context.Context, q RowQuerier) ([]memoryRow, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, memorySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var collected []memoryRow

	for rows.Next() {
		var row memoryRow

		if err := rows.Scan(&row.name, &row.off, &row.size, &row.allocatedSize); err != nil {
			return nil, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return collected, nil
}

func memoryCells(rows []memoryRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{text(row.name), int64Text(row.off), int64Text(row.size), int64Text(row.allocatedSize)}
	}

	return cells
}
