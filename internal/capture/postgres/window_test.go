package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testWindowStart = time.Date(2026, 8, 7, 14, 32, 4, 980_000_000, time.UTC)

func testWindowTarget() Target {
	target := testTarget()
	target.Database = "orders_configured"

	return target
}

type fakeClock struct {
	current time.Time

	waits []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{current: testWindowStart} }

func (c *fakeClock) now() time.Time { return c.current }

func (c *fakeClock) advance(d time.Duration) { c.current = c.current.Add(d) }

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)

	if d > 0 {
		c.advance(d)
	}

	fired := make(chan time.Time, 1)
	fired <- c.current

	return fired
}

type fakeCollector struct {
	artifact Artifact
	sample   func(ctx context.Context, s SampleContext, w io.Writer) error

	seen      []SampleContext
	deadlines []time.Time
}

func newFakeCollector(name string) *fakeCollector {
	return &fakeCollector{artifact: Artifact{
		Name:     name,
		FileName: name + ".txt",
		Scope:    "database",
		Schedule: ScheduleStartEnd,
	}}
}

func (f *fakeCollector) Artifact() Artifact { return f.artifact }

func (f *fakeCollector) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	f.seen = append(f.seen, s)

	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)

	if f.sample != nil {
		return f.sample(ctx, s, w)
	}

	if err := writeBlockHeader(w, "fake_view", f.artifact.Scope, []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}, s.At); err != nil {
		return err
	}

	return writeRows(w, []string{"relid"}, [][]string{{"16390"}})
}

type fakeWindowConn struct {
	database    string
	dbid        *string
	identifyErr error

	closed bool
}

func newFakeWindowConn() *fakeWindowConn {
	return &fakeWindowConn{database: "orders_db", dbid: ptr("16401")}
}

func (c *fakeWindowConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql != currentDatabaseSQL {
		return fakeRow{err: fmt.Errorf("unexpected query: %s", sql)}
	}

	if c.identifyErr != nil {
		return fakeRow{err: c.identifyErr}
	}

	return fakeRow{values: []any{c.database, c.dbid}}
}

func (c *fakeWindowConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, errors.New("the window reads no row sets; its collectors do")
}

func (c *fakeWindowConn) Close(ctx context.Context) error {
	c.closed = true
	return nil
}

func newTestWindow(t *testing.T, clock *fakeClock, collectors ...Collector) *Window {
	t.Helper()
	t.Chdir(t.TempDir())

	return &Window{
		Target:     testWindowTarget(),
		Duration:   120 * time.Second,
		Collectors: collectors,
		now:        clock.now,
		after:      clock.after,
		connect: func(ctx context.Context, target Target) (windowConn, error) {
			return newFakeWindowConn(), nil
		},
	}
}

func artifactText(t *testing.T, result ArtifactResult) string {
	t.Helper()

	content, err := os.ReadFile(result.Artifact.FileName)
	require.NoError(t, err)

	return string(content)
}

func headersOf(t *testing.T, result ArtifactResult) []string {
	t.Helper()

	var headers []string
	for line := range strings.SplitSeq(strings.TrimSuffix(artifactText(t, result), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			headers = append(headers, line)
		}
	}

	return headers
}

func TestWindowStartEndWritesTwoSamples(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	results := newTestWindow(t, clock, collector).Run(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 2, results[0].SamplesExpected)
	assert.Equal(t, 2, results[0].SamplesWritten)
	assert.NoError(t, results[0].IOErr)

	require.Len(t, collector.seen, 2)
	assert.Equal(t, 1, collector.seen[0].Index)
	assert.Equal(t, 2, collector.seen[1].Index)

	assert.Equal(t, testWindowStart, collector.seen[0].At, "sample 1 is at t0")
	assert.Equal(t, testWindowStart.Add(120*time.Second), collector.seen[1].At,
		"sample 2 is at t0+duration")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 4, "preamble, two samples, closing block")
	assert.Contains(t, headers[0], "status=started window=120s samples_expected=2")
	assert.Contains(t, headers[1], "sample=1")
	assert.Contains(t, headers[2], "sample=2")
	assert.Contains(t, headers[3], "status=complete samples_expected=2 samples_written=2")
}

func TestWindowSecondSampleTargetsTheWindowEndNotTheSampleEnd(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			clock.advance(30 * time.Second)
		}
		return nil
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	require.Equal(t, []time.Duration{90 * time.Second}, clock.waits,
		"the wait is duration minus elapsed, not the whole duration")

	assert.Equal(t, testWindowStart.Add(120*time.Second), collector.seen[1].At,
		"sample 2 still lands at t0+duration")
	assert.Equal(t, StatusComplete, results[0].Status)
}

func TestWindowDoesNotWaitWhenTheFirstSampleOverranTheWindow(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			clock.advance(200 * time.Second)
		}
		return nil
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	require.Len(t, clock.waits, 1)
	assert.Negative(t, clock.waits[0], "the wait is already in the past")
	assert.Equal(t, 2, results[0].SamplesWritten, "sample 2 still happens, immediately")
}

func TestWindowWritesThePreambleBeforeConnecting(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	var atConnect string
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		content, err := os.ReadFile(collector.artifact.FileName)
		require.NoError(t, err, "the artifact must exist before the connection is attempted")
		atConnect = string(content)

		return newFakeWindowConn(), nil
	}

	window.Run(context.Background())

	assert.Contains(t, atConnect, "status=started",
		"the preamble must be on disk, and synced, before Connect")
	assert.Contains(t, atConnect, "samples_expected=2",
		"a file that stops here still says how many samples were meant to be in it")
}

func TestWindowPreambleCarriesTheConfiguredDatabaseAndNoOID(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	results := newTestWindow(t, clock, collector).Run(context.Background())

	headers := headersOf(t, results[0])
	assert.Contains(t, headers[0], "db=orders_configured dbid= status=started")
	assert.Contains(t, headers[1], "db=orders_db dbid=16401",
		"every block after the connection carries current_database() and its OID")
	assert.Contains(t, headers[3], "db=orders_db dbid=16401",
		"including the closing block")
}

func TestWindowConnectFailureDoesNotCostTheWindow(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		return nil, ErrTooManyConnections
	}

	results := window.Run(context.Background())

	assert.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, 0, results[0].SamplesWritten)
	assert.Empty(t, collector.seen, "a refused connection takes no samples")
	assert.Empty(t, clock.waits, "and does not wait out the window")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 2, "a connect failure is two lines: what was attempted, and why not")
	assert.Contains(t, headers[1],
		"db=orders_configured dbid= status=connect_failed "+
			"samples_expected=2 samples_written=0 connect_error=too_many_connections")

	assert.NotEmpty(t, artifactText(t, results[0]),
		"the file is never zero bytes, so the upload path cannot drop it")
}

func TestWindowCancelledBetweenSamples(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(sampleCtx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			cancel()
		}
		return nil
	}

	results := newTestWindow(t, clock, collector).Run(ctx)

	assert.Equal(t, StatusCancelled, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Len(t, collector.seen, 1)

	headers := headersOf(t, results[0])
	assert.Contains(t, headers[len(headers)-1],
		"status=cancelled samples_expected=2 samples_written=1")
}

func TestWindowDeadlineExceededIsNotCancelled(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	window.Duration = 10 * time.Millisecond
	window.grace = 200 * time.Millisecond

	window.after = func(d time.Duration) <-chan time.Time { return time.After(time.Minute) }

	results := window.Run(context.Background())

	assert.Equal(t, StatusDeadlineExceeded, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)

	headers := headersOf(t, results[0])
	assert.Contains(t, headers[len(headers)-1],
		"status=deadline_exceeded samples_expected=2 samples_written=1")
}

func TestWindowWritesTheClosingBlockWhenTheContextIsAlreadyDone(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := newTestWindow(t, clock, collector).Run(ctx)

	assert.Equal(t, StatusCancelled, results[0].Status)
	assert.Equal(t, 0, results[0].SamplesWritten)

	headers := headersOf(t, results[0])
	require.Len(t, headers, 2)
	assert.Contains(t, headers[1], "status=cancelled samples_expected=2 samples_written=0")
}

func TestWindowModuleDeadlineIsTheWindowPlusGrace(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	var connectDeadline time.Time
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		connectDeadline, _ = ctx.Deadline()
		return newFakeWindowConn(), nil
	}

	before := time.Now()
	window.Run(context.Background())
	after := time.Now()

	require.NotEmpty(t, collector.deadlines)
	assert.WithinRange(t, collector.deadlines[0],
		before.Add(window.Duration+WindowGrace), after.Add(window.Duration+WindowGrace),
		"the module deadline is the window plus the grace")

	assert.WithinRange(t, connectDeadline,
		before.Add(ConnectTimeout), after.Add(ConnectTimeout))
}

func TestWindowModuleDeadlineExcludesTheConnect(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	var connectReturned time.Time
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		time.Sleep(100 * time.Millisecond)
		connectReturned = time.Now()

		return newFakeWindowConn(), nil
	}

	window.Run(context.Background())

	require.NotEmpty(t, collector.deadlines)
	assert.False(t, collector.deadlines[0].Before(connectReturned.Add(window.Duration+WindowGrace)),
		"the deadline was armed before connecting, spending grace the final sample needs")
}

func TestWindowWritesTheStubBlockForAFailedSample(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			return errors.New("ERROR: canceling statement due to statement timeout")
		}
		return writeRows(w, []string{"relid"}, [][]string{{"16390"}})
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	assert.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten, "only complete samples are counted")
	assert.Len(t, collector.seen, 2, "a failed sample does not stop the window")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 3, "preamble, the stub, and the closing block")

	assert.Contains(t, headers[1], "source=pg_fake",
		"the stub names the artifact, not the view: it is about the sampling")
	assert.Contains(t, headers[1],
		`sample=1 sample_error="ERROR: canceling statement due to statement timeout"`,
		"driver text is quoted, so it cannot break k=v tokenisation")

	assert.Contains(t, headers[2], "status=partial samples_expected=2 samples_written=1")
}

func TestWindowRedactsThePasswordFromASampleError(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		return fmt.Errorf("connection string was postgres://u:%s@h/db", testWindowTarget().Password)
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	assert.NotContains(t, artifactText(t, results[0]), testWindowTarget().Password)
	assert.NotContains(t, results[0].Err, testWindowTarget().Password)
	assert.Contains(t, artifactText(t, results[0]), "<redacted>")
}

func TestWindowCreateFailureIsRecordedAndIsolated(t *testing.T) {
	clock := newFakeClock()

	blocked := newFakeCollector("pg_blocked")
	healthy := newFakeCollector("pg_healthy")

	window := newTestWindow(t, clock, blocked, healthy)

	require.NoError(t, os.Mkdir(blocked.artifact.FileName, 0o755))

	results := window.Run(context.Background())

	require.Len(t, results, 2)

	assert.Error(t, results[0].IOErr, "the one failure the artifact cannot record about itself")
	assert.Nil(t, results[0].File)
	assert.Equal(t, 0, results[0].SamplesWritten)
	assert.Empty(t, blocked.seen, "a collector with nowhere to write is not asked to sample")

	assert.NoError(t, results[1].IOErr, "one artifact's I/O failure does not affect another's")
	assert.Equal(t, StatusComplete, results[1].Status)
	assert.Equal(t, 2, results[1].SamplesWritten)
}

func TestWindowRunsEveryCollectorIntoItsOwnFile(t *testing.T) {
	clock := newFakeClock()

	first := newFakeCollector("pg_first")
	second := newFakeCollector("pg_second")

	results := newTestWindow(t, clock, first, second).Run(context.Background())

	require.Len(t, results, 2)
	for _, result := range results {
		assert.Equal(t, StatusComplete, result.Status)
		assert.Equal(t, 2, result.SamplesWritten)
		assert.Len(t, headersOf(t, result), 4)
	}

	assert.Contains(t, artifactText(t, results[0]), "source=pg_first")
	assert.Contains(t, artifactText(t, results[1]), "source=pg_second")
	assert.NotContains(t, artifactText(t, results[0]), "pg_second")
}

func TestWindowOneCollectorsFailureDoesNotAffectAnother(t *testing.T) {
	clock := newFakeClock()

	failing := newFakeCollector("pg_failing")
	failing.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		return errors.New("ERROR: permission denied for view pg_stat_user_tables")
	}
	healthy := newFakeCollector("pg_healthy")

	results := newTestWindow(t, clock, failing, healthy).Run(context.Background())

	assert.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 0, results[0].SamplesWritten)

	assert.Equal(t, StatusComplete, results[1].Status)
	assert.Equal(t, 2, results[1].SamplesWritten)
}

func TestWindowFallsBackToTheConfiguredDatabaseWhenIdentifyFails(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		conn := newFakeWindowConn()
		conn.identifyErr = errors.New("ERROR: permission denied")

		return conn, nil
	}

	results := window.Run(context.Background())

	assert.Equal(t, StatusComplete, results[0].Status,
		"an unidentified database is still a captured one")
	assert.Equal(t, "orders_configured", collector.seen[0].Database)
	assert.Empty(t, collector.seen[0].DBID)
}

func TestWindowClosesTheConnection(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	conn := newFakeWindowConn()

	window := newTestWindow(t, clock, collector)
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		return conn, nil
	}

	window.Run(context.Background())

	assert.True(t, conn.closed, "the window returns its slot")
}

func TestWindowWithNoCollectorsIsHarmless(t *testing.T) {
	clock := newFakeClock()

	assert.Empty(t, newTestWindow(t, clock).Run(context.Background()))
}

func TestStoppedStatusDistinguishesTheDeadlineFromTheCaller(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	assert.Equal(t, StatusDeadlineExceeded, stoppedStatus(expired))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, StatusCancelled, stoppedStatus(cancelled))
}

func TestWindowSecondsRendersForAMachineReader(t *testing.T) {
	assert.Equal(t, "120s", windowSeconds(120*time.Second))
	assert.Equal(t, "600s", windowSeconds(600*time.Second))
	assert.Equal(t, "10s", windowSeconds(10*time.Second))
	assert.Equal(t, "90.5s", windowSeconds(90500*time.Millisecond),
		"a window that is not whole seconds keeps its precision")
}

func TestScheduleSampleCount(t *testing.T) {
	assert.Equal(t, 2, ScheduleStartEnd.samples())
}
