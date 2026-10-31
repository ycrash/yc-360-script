package dbcore

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The window's own behaviour is pinned by the postgres package's window tests,
// which run it through that engine. These pin what an engine parameterises.

var testStart = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

type fakeFacts struct{ schema string }

type fakeQuerier struct{ closed bool }

func (q *fakeQuerier) Close(context.Context) error {
	q.closed = true

	return nil
}

type fakeCollector struct {
	artifact Artifact
	err      error
}

func (c fakeCollector) Artifact() Artifact { return c.artifact }

func (c fakeCollector) Sample(_ context.Context, _ *fakeQuerier, w io.Writer, s SampleContext[fakeFacts]) error {
	if c.err != nil {
		return c.err
	}

	return WriteSampleHeader(w, "testdb", testLocation(s), c.artifact, s, SampleHeader{
		Source: c.artifact.Name, Status: StatusOK,
	})
}

func testLocation(s SampleContext[fakeFacts]) []HeaderField {
	return []HeaderField{{"db", s.Database}, {"schema", s.Engine.schema}}
}

func testEngine(dial func(context.Context) (*fakeQuerier, Closer, error)) Engine[*fakeQuerier, fakeFacts] {
	return Engine[*fakeQuerier, fakeFacts]{
		Name: "testdb",
		Dial: dial,
		Identify: func(_ context.Context, _ *fakeQuerier, base SampleContext[fakeFacts]) SampleContext[fakeFacts] {
			base.Database = "orders"
			base.EngineVersion = "8.4.3"
			base.Engine.schema = "orders_schema"

			return base
		},
		Location:            testLocation,
		ErrorText:           func(err error) string { return RedactError(err, "s3cret") },
		ConnectErrorText:    func(err error) string { return "refused" },
		ConnectTimeout:      time.Second,
		DefaultSampleBudget: time.Second,
		CloseMargin:         time.Second,
	}
}

func runTestWindow(t *testing.T, engine Engine[*fakeQuerier, fakeFacts], collectors ...Collector[*fakeQuerier, fakeFacts]) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	now := testStart

	window := &Window[*fakeQuerier, fakeFacts]{
		Engine:     engine,
		Duration:   10 * time.Second,
		Collectors: collectors,
		CaptureID:  "capture-1",
		TargetID:   "orders-primary",
		Database:   "configured",
		Seams: Seams{
			Now: func() time.Time { return now },
			After: func(d time.Duration) <-chan time.Time {
				now = now.Add(d)
				ch := make(chan time.Time, 1)
				ch <- now

				return ch
			},
		},
	}

	results := window.Run(context.Background())
	for _, result := range results {
		if result.File != nil {
			t.Cleanup(func() { result.File.Close() })
		}
	}

	return results
}

func fileLines(t *testing.T, result ArtifactResult) []string {
	t.Helper()

	content, err := os.ReadFile(result.Artifact.FileName)
	require.NoError(t, err)

	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}

func TestWindowWritesTheEnginesNameAndLocationOnEveryHeader(t *testing.T) {
	conn := &fakeQuerier{}
	engine := testEngine(func(context.Context) (*fakeQuerier, Closer, error) { return conn, conn, nil })

	results := runTestWindow(t, engine,
		fakeCollector{artifact: Artifact{Name: "t_ok", FileName: "t_ok.txt", Scope: "cluster", Schedule: Periodic(5 * time.Second)}},
		fakeCollector{
			artifact: Artifact{Name: "t_fail", FileName: "t_fail.txt", Scope: "cluster", Schedule: StartEnd()},
			err:      errors.New("denied for s3cret"),
		},
	)

	ok := fileLines(t, results[0])
	assert.Equal(t, "# engine=testdb source=t_ok v=1 format=csv scope=cluster db=configured schema= status=started "+
		"window=10s schedule=periodic interval=5s samples_expected=3 ts=2026-10-27T09:00:00.000Z", ok[0],
		"the preamble is written before the connection names its database")
	assert.Equal(t, "# capture_id=capture-1 target_id=orders-primary engine=testdb engine_version=8.4.3 v=1 "+
		"format=csv db=orders schema=orders_schema", ok[1])
	assert.Equal(t, "# engine=testdb source=t_ok v=1 format=csv scope=cluster db=orders schema=orders_schema "+
		"status=complete samples_expected=3 samples_written=3 samples_skipped=0 ts=2026-10-27T09:00:10.000Z", ok[len(ok)-1])

	failed := fileLines(t, results[1])
	assert.Equal(t, "# engine=testdb source=t_fail v=1 format=csv scope=cluster db=orders schema=orders_schema "+
		`sample=1 sample_error="denied for <redacted>" ts=2026-10-27T09:00:00.000Z`, failed[1],
		"a stub carries the engine's location and its redaction")

	assert.True(t, conn.closed)
}

func TestWindowDialsAgainOnlyAfterARefusedSlot(t *testing.T) {
	dials := 0
	slotRefused := errors.New("too many connections")

	engine := testEngine(func(context.Context) (*fakeQuerier, Closer, error) {
		dials++

		return nil, nil, slotRefused
	})

	fast := fakeCollector{artifact: Artifact{Name: "f", FileName: "f.txt", Schedule: Once(), Connection: ConnectionFast}}
	slow := fakeCollector{artifact: Artifact{Name: "s", FileName: "s.txt", Schedule: Once(), Connection: ConnectionExpensive}}

	results := runTestWindow(t, engine, slow, fast)
	assert.Equal(t, 1, dials, "a refusal the engine does not call a slot refusal is the target's")
	assert.Equal(t, StatusConnectFailed, results[0].Status)

	dials = 0
	engine.SlotRefused = func(err error) bool { return errors.Is(err, slotRefused) }

	runTestWindow(t, engine, slow, fast)
	assert.Equal(t, 2, dials, "a refused slot fails only its own connection")
}

// losingConn is a connection the driver has closed once lost is set.
type losingConn struct {
	fakeQuerier

	lost bool
}

func (c *losingConn) Lost() bool { return c.lost }

func (c *losingConn) LossError() error { return errors.New("connection reset by peer") }

// losingCollector loses its connection in its first sample.
type losingCollector struct {
	fakeCollector

	conn *losingConn
}

func (c losingCollector) Sample(_ context.Context, _ *fakeQuerier, _ io.Writer, _ SampleContext[fakeFacts]) error {
	c.conn.lost = true

	return errors.New("connection reset by peer")
}

func TestWindowKeepsAFileFinishedBeforeALossCompleteOnlyWhereTheEngineSaysSo(t *testing.T) {
	for _, keep := range []bool{false, true} {
		conn := &losingConn{}

		engine := testEngine(func(context.Context) (*fakeQuerier, Closer, error) { return &conn.fakeQuerier, conn, nil })
		engine.KeepsFinishedOnLoss = keep

		results := runTestWindow(t, engine,
			fakeCollector{artifact: Artifact{Name: "t_once", FileName: "t_once.txt", Schedule: Once()}},
			losingCollector{
				fakeCollector: fakeCollector{artifact: Artifact{Name: "t_lost", FileName: "t_lost.txt", Schedule: Periodic(5 * time.Second)}},
				conn:          conn,
			},
		)

		assert.Equal(t, StatusConnectionLost, results[1].Status, "the file the loss cut short")

		once := fileLines(t, results[0])
		if !keep {
			assert.Equal(t, StatusConnectionLost, results[0].Status, "off, the loss overrides every file on the connection")
			assert.Contains(t, once[len(once)-1], `connection_error="connection reset by peer"`)

			continue
		}

		assert.Equal(t, StatusComplete, results[0].Status)
		assert.Empty(t, results[0].Err)
		assert.Contains(t, once[len(once)-1], "status=complete samples_expected=1 samples_written=1")
		assert.NotContains(t, once[len(once)-1], "connection_error=")
	}
}
