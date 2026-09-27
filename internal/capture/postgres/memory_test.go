package postgres

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	colMemoryName = iota
	colMemoryOff
	colMemorySize
	colMemoryAllocatedSize
)

func memoryRowFor(name *string, off *int64, size, allocated int64) []any {
	return []any{name, off, counted(size), counted(allocated)}
}

// Largest first, as the statement orders them. Shared memory is sized at
// startup, so the two samples match; the unnamed row is what is left
// unallocated, and <anonymous> has no one offset.
func memorySample() [][]any {
	return [][]any{
		memoryRowFor(nullable("Buffer Blocks"), counted(6779904), 1073741824, 1073741824),
		memoryRowFor(nullable("<anonymous>"), nil, 7017216, 7017216),
		memoryRowFor(nullable("XLOG Ctl"), counted(55168), 16803400, 16803456),
		memoryRowFor(nil, counted(1098842112), 1892864, 1892864),
		memoryRowFor(nullable("Buffer Descriptors"), counted(5731328), 8388608, 8388608),
		memoryRowFor(nullable("Checkpointer Data"), counted(1095617536), 3932216, 3932288),
		memoryRowFor(nullable("Shared Memory Stats"), counted(1096295424), 279992, 280064),
		memoryRowFor(nullable("subtransaction"), counted(5061376), 267424, 267520),
	}
}

type fakeMemoryConn struct {
	*fakeWindowConn

	allowed []fakeRow
	memory  []fakeResult
	args    [][]any
	sql     []string
}

func newFakeMemoryConn() *fakeMemoryConn {
	return &fakeMemoryConn{
		fakeWindowConn: newFakeWindowConn(),
		allowed:        repeatRow(rowResult(ptr(true))),
		memory:         repeat(rowsResult(memorySample())),
	}
}

func (c *fakeMemoryConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == memoryPrivilegeSQL {
		c.sql = append(c.sql, sql)

		return answerRow(&c.allowed)
	}

	return c.fakeWindowConn.QueryRow(ctx, sql, args...)
}

func (c *fakeMemoryConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql != memorySQL {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	c.sql = append(c.sql, sql)
	c.args = append(c.args, args)

	return answer(&c.memory)
}

func permissionDenied42501() error {
	return &pgconn.PgError{
		Severity: "ERROR",
		Code:     insufficientPrivilegeCode,
		Message:  "permission denied for view pg_shmem_allocations",
	}
}

func runMemoryWindow(t *testing.T, clock *scriptedClock,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:         testTarget(),
		Duration:       120 * time.Second,
		Collectors:     []Collector{Memory{}},
		now:            clock.now,
		CaptureID:      testCaptureID,
		statementClock: steppedStatements,
		after:          clock.after,
		connect:        connect,
	}

	return window.Run(context.Background())
}

// memoryBlock splits one sample into its header's fields and its rows, the
// column header checked and dropped.
func memoryBlock(t *testing.T, block string) (map[string]string, [][]string) {
	t.Helper()

	var (
		header string
		body   strings.Builder
	)

	for line := range strings.SplitSeq(block, "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			header = line
		case line != "":
			body.WriteString(line)
			body.WriteString("\n")
		}
	}

	records, err := csv.NewReader(strings.NewReader(body.String())).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records)
	require.Equal(t, memoryColumns, records[0], "the column header leads every block")

	return headerFields(t, header), records[1:]
}

func takeMemorySample(t *testing.T, conn *fakeMemoryConn) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, Memory{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	return buf.String()
}

func TestMemoryArtifact(t *testing.T) {
	artifact := Memory{}.Artifact()

	assert.Equal(t, "pg_memory", artifact.Name)
	assert.Equal(t, "pg_memory.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope, "the server's shared memory, whichever database is connected")
	assert.Equal(t, Periodic(0), artifact.Schedule,
		"no cadence given is the bookend alone, never a single sample")
	assert.Equal(t, Periodic(15*time.Second), Memory{Interval: 15 * time.Second}.Artifact().Schedule,
		"the run's cadence, with the close as the last sample")
	assert.Zero(t, artifact.SampleBudget,
		"the privilege check and the read: DefaultSampleBudget's two statements")
}

func TestMemoryColumnOrder(t *testing.T) {
	assert.Equal(t, []string{"name", "off", "size", "allocated_size"}, memoryColumns,
		"the view's own names, in this order")
	assert.Equal(t, "name", memoryColumns[colMemoryName], "the key leads")
}

func TestMemoryStatementReadsEveryAllocationLargestFirst(t *testing.T) {
	assert.Contains(t, memorySQL, "SELECT name, off, size, allocated_size")
	assert.Contains(t, memorySQL, "FROM pg_catalog.pg_shmem_allocations")
	assert.Contains(t, memorySQL, "ORDER BY allocated_size DESC")
	assert.NotContains(t, memorySQL, "LIMIT", "no cap: every row is captured")
	assert.NotContains(t, memorySQL, "WHERE", "and no filter")

	conn := newFakeMemoryConn()
	takeMemorySample(t, conn)

	require.Len(t, conn.args, 1)
	assert.Empty(t, conn.args[0], "the statement takes no arguments")
}

func TestMemoryGoldenFull(t *testing.T) {
	results := runMemoryWindow(t, goldenClock(t), connectTo(newFakeMemoryConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_memory_full.txt"), artifactText(t, results[0]))
}

func TestMemoryGoldenPermissionDenied(t *testing.T) {
	conn := newFakeMemoryConn()
	conn.allowed = repeatRow(rowResult(ptr(false)))

	results := runMemoryWindow(t, goldenClock(t), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"a refusal is a reading of what this role may see, not a failed sample")
	assert.Equal(t, 2, results[0].SamplesWritten)
	assert.Empty(t, results[0].Err, "and the run's message carries no sample error")
	assert.Equal(t, bloatGolden(t, "pg_memory_permission_denied.txt"), artifactText(t, results[0]))
}

func TestMemoryGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runMemoryWindow(t, clock,
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_memory_connect_failure.txt"), artifactText(t, results[0]))
}

func TestMemoryGoldenSampleError(t *testing.T) {
	clock := newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 15, 201),
		at(32, 15, 201),
		at(34, 5, 140),
		at(34, 5, 201),
	)

	conn := newFakeMemoryConn()
	conn.memory = queue(
		errResult(statementTimedOut()),
		rowsResult(memorySample()),
	)

	results := runMemoryWindow(t, clock, connectTo(conn))

	require.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_memory_sample_error.txt"), artifactText(t, results[0]))
}

func TestMemorySkipsTheReadTheRoleWouldBeRefused(t *testing.T) {
	assert.Equal(t, `SELECT has_table_privilege('pg_catalog.pg_shmem_allocations', 'SELECT')`, memoryPrivilegeSQL)

	conn := newFakeMemoryConn()
	conn.allowed = repeatRow(rowResult(ptr(false)))

	header, rows := memoryBlock(t, takeMemorySample(t, conn))

	assert.Equal(t, []string{memoryPrivilegeSQL}, conn.sql,
		"a refused read is an ERROR and its STATEMENT in the server's own log, every sample")
	assert.Equal(t, "permission_denied", header["reason"])
	assert.NotContains(t, header, "error", "a refusal is not an error")
	assert.Empty(t, rows, "the column header alone")
}

func TestMemoryChecksThePrivilegeBeforeEveryRead(t *testing.T) {
	conn := newFakeMemoryConn()

	takeMemorySample(t, conn)
	takeMemorySample(t, conn)

	assert.Equal(t, []string{memoryPrivilegeSQL, memorySQL, memoryPrivilegeSQL, memorySQL}, conn.sql,
		"asked on every sample, so a grant made during the window is seen at the next one")
}

func TestMemoryReadIsTriedWhenTheCheckAnswersNULL(t *testing.T) {
	conn := newFakeMemoryConn()
	conn.allowed = repeatRow(rowResult(nil))

	_, rows := memoryBlock(t, takeMemorySample(t, conn))

	assert.Equal(t, []string{memoryPrivilegeSQL, memorySQL}, conn.sql, "only a false skips the read")
	assert.Len(t, rows, 8)
}

func TestMemoryFailedPrivilegeCheckIsAFailedSample(t *testing.T) {
	conn := newFakeMemoryConn()
	conn.allowed = repeatRow(errRow(errors.New("ERROR: canceling statement due to statement timeout")))

	var buf bytes.Buffer
	err := Memory{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	})

	require.Error(t, err)
	assert.Equal(t, []string{memoryPrivilegeSQL}, conn.sql, "nothing is known, so nothing is tried")
	assert.Empty(t, buf.String(), "the window writes the stub")
}

func TestMemoryRefusalAfterTheCheckIsStillAReason(t *testing.T) {
	conn := newFakeMemoryConn()
	conn.memory = repeat(errResult(fmt.Errorf("sample: %w", permissionDenied42501())))

	header, rows := memoryBlock(t, takeMemorySample(t, conn))

	assert.Equal(t, "permission_denied", header["reason"],
		"a grant revoked between the check and the read, found through a wrapped error too")
	assert.NotContains(t, header, "error", "a refusal is not an error")
	assert.Equal(t, "pg_shmem_allocations", header["source"])
	assert.Empty(t, rows, "the column header alone")
}

func TestMemoryOtherFailuresWriteNothing(t *testing.T) {
	for name, err := range map[string]error{
		"a statement timeout": errors.New("ERROR: canceling statement due to statement timeout"),
		"another SQLSTATE":    &pgconn.PgError{Severity: "ERROR", Code: "57014", Message: "canceling statement"},
	} {
		t.Run(name, func(t *testing.T) {
			conn := newFakeMemoryConn()
			conn.memory = repeat(errResult(err))

			var buf bytes.Buffer
			sampleErr := Memory{}.Sample(context.Background(), conn, &buf, SampleContext{
				At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
			})

			require.Error(t, sampleErr)
			assert.Empty(t, buf.String(), "a failed sample leaves the artifact untouched: the window writes the stub")
		})
	}
}

func TestMemoryWritesEveryAllocationInTheStatementsOrder(t *testing.T) {
	header, rows := memoryBlock(t, takeMemorySample(t, newFakeMemoryConn()))
	require.Len(t, rows, 8)

	assert.NotContains(t, header, "reason", "a reading carries no reason")
	assert.Equal(t, []string{"Buffer Blocks", "6779904", "1073741824", "1073741824"}, rows[0], "largest first")
	assert.Equal(t, []string{"<anonymous>", "", "7017216", "7017216"}, rows[1],
		"the anonymous allocations have no one offset: an empty cell, never 0")
	assert.Equal(t, []string{"", "1098842112", "1892864", "1892864"}, rows[3],
		"the unallocated remainder has no name")
}

func TestMemoryWritesTheBlockInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, Memory{}.Sample(context.Background(), newFakeMemoryConn(), writer, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	assert.Equal(t, 1, writer.writes,
		"a write failing between header and body would leave the window's stub behind a half-written block")
	assert.NotEmpty(t, writer.buf.String())
}
