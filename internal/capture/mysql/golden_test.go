package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yc-agent/internal/capture/dbcore"
)

// Goldens replay testdata/my84_*.json, recorded by TestRecordGoldens (-tags mysqlintegration,
// YC_MY_RECORD=1). YC_UPDATE_GOLDEN=1 rewrites them.

// Must match runMatrixWindow's.
const (
	goldenWindow    = 12 * time.Second
	goldenFast      = 4 * time.Second
	goldenNormal    = 6 * time.Second
	goldenExpensive = 6 * time.Second
	goldenExplain   = "all"
	goldenCaptureID = "golden-capture"
)

var goldenStart = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

func goldenTarget(user string) Target {
	return Target{ID: "my84", Host: "127.0.0.1", Port: 13384, Username: user}
}

func goldenCollectors(target Target, agentNow time.Time) []Collector {
	metadata := NewMetadata(target, "golden", agentNow, goldenExplain)
	metadata.inspect = fakeProcesses{foreignVisible: true}

	return Collectors(metadata, goldenExplain, goldenFast, goldenNormal, goldenExpensive)
}

type recording struct {
	Server     string              `json:"server"`
	Role       string              `json:"role"`
	Statements []recordedStatement `json:"statements"`
}

type recordedStatement struct {
	Query   string         `json:"query"`
	Columns []string       `json:"columns,omitempty"`
	Rows    [][]*string    `json:"rows,omitempty"`
	Error   *recordedError `json:"error,omitempty"`
}

// Number 0 is a client error.
type recordedError struct {
	Number  uint16 `json:"number"`
	Message string `json:"message"`
}

func (e *recordedError) err() error {
	if e.Number == 0 {
		return errors.New(e.Message)
	}

	return serverError(e.Number, e.Message)
}

func loadRecording(t *testing.T, name string) recording {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	var r recording
	require.NoError(t, json.Unmarshal(content, &r))

	return r
}

// replayQuerier repeats the last answer once a statement's recordings run out.
type replayQuerier struct {
	mu      sync.Mutex
	answers map[string][]recordedStatement
	served  map[string]int
}

func newReplay(r recording) *replayQuerier {
	q := &replayQuerier{answers: map[string][]recordedStatement{}, served: map[string]int{}}

	for _, statement := range r.Statements {
		q.answers[statement.Query] = append(q.answers[statement.Query], statement)
	}

	return q
}

func (q *replayQuerier) Query(_ context.Context, query string) (Rows, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	answers := q.answers[query]
	if len(answers) == 0 {
		return nil, fmt.Errorf("not in the recording: %s", query)
	}

	answer := answers[min(q.served[query], len(answers)-1)]
	q.served[query]++

	if answer.Error != nil {
		return nil, answer.Error.err()
	}

	return &replayRows{columns: answer.Columns, rows: answer.Rows}, nil
}

type replayRows struct {
	columns []string
	rows    [][]*string
	at      int
}

func (r *replayRows) Columns() ([]string, error) { return r.columns, nil }

func (r *replayRows) Next() bool {
	r.at++

	return r.at <= len(r.rows)
}

func (r *replayRows) Scan(dest ...any) error {
	row := r.rows[r.at-1]
	if len(row) != len(dest) {
		return errors.New("scan: column count")
	}

	for i, value := range row {
		target, ok := dest[i].(*sql.NullString)
		if !ok {
			return errors.New("scan: destination is not a NullString")
		}

		*target = sql.NullString{}
		if value != nil {
			*target = sql.NullString{String: *value, Valid: true}
		}
	}

	return nil
}

func (r *replayRows) Err() error   { return nil }
func (r *replayRows) Close() error { return nil }

type replayConn struct{}

func (replayConn) Close(context.Context) error    { return nil }
func (replayConn) ConnectDuration() time.Duration { return 1500 * time.Microsecond }

// goldenClock passes a wait at once and moves on.
type goldenClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *goldenClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *goldenClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)

	fired := make(chan time.Time, 1)
	fired <- c.now

	return fired
}

func goldenStatements(at time.Time) func() time.Time {
	reads := 0

	return func() time.Time {
		reads++

		return at.Add(time.Duration(reads) * time.Millisecond)
	}
}

func replayRun(t *testing.T, r recording, target Target, dial func(context.Context) (Querier, dbcore.Closer, error)) map[string]string {
	t.Helper()
	t.Chdir(t.TempDir())

	if dial == nil {
		replay := newReplay(r)
		dial = func(context.Context) (Querier, dbcore.Closer, error) { return replay, replayConn{}, nil }
	}

	collectors := goldenCollectors(target, goldenStart)

	var connections []string

	byConnection := map[string][]Collector{}

	for _, c := range collectors {
		connection := c.Artifact().Connection
		if byConnection[connection] == nil {
			connections = append(connections, connection)
		}

		byConnection[connection] = append(byConnection[connection], c)
	}

	files := map[string]string{}

	for _, connection := range connections {
		clock := &goldenClock{now: goldenStart}

		window := &Window{
			Engine:     engine(target, dial),
			Duration:   goldenWindow,
			Collectors: byConnection[connection],
			CaptureID:  goldenCaptureID,
			TargetID:   target.id(),
			Database:   target.Database,
			Seams:      dbcore.Seams{Now: clock.Now, After: clock.After, StatementClock: goldenStatements},
		}

		for _, result := range window.Run(context.Background()) {
			if result.File != nil {
				result.File.Close()
			}

			content, err := os.ReadFile(result.Artifact.FileName)
			require.NoError(t, err)

			files[result.Artifact.FileName] = string(content)
		}
	}

	return files
}

func assertGoldens(t *testing.T, files map[string]string, regime string, only ...string) {
	t.Helper()

	update := os.Getenv("YC_UPDATE_GOLDEN") != ""

	for name, content := range files {
		if len(only) > 0 && !slices.Contains(only, name) {
			continue
		}

		golden := filepath.Join(packageDir, "testdata", strings.TrimSuffix(name, ".txt")+"_"+regime+".txt")

		if update {
			require.NoError(t, os.WriteFile(golden, []byte(content), 0o644))

			continue
		}

		want, err := os.ReadFile(golden)
		require.NoError(t, err, "no golden: YC_UPDATE_GOLDEN=1 writes it")
		assert.Equal(t, string(want), content, golden)
	}
}

// Read before a test changes directory.
var packageDir, _ = os.Getwd()

func TestGoldenFull(t *testing.T) {
	r := loadRecording(t, "my84_root.json")

	files := replayRun(t, r, goldenTarget("root"), nil)

	require.Len(t, files, 16, "every my_* file")
	assertGoldens(t, files, "full")
}

func TestGoldenLeastPrivilege(t *testing.T) {
	r := loadRecording(t, "my84_restricted.json")

	files := replayRun(t, r, goldenTarget("yc_restricted"), nil)

	require.Len(t, files, 16)
	assertGoldens(t, files, "least_privilege")
}

func TestGoldenConnectFailure(t *testing.T) {
	refused := func(context.Context) (Querier, dbcore.Closer, error) {
		return nil, nil, fmt.Errorf("%w: %w", ErrTooManyConnections, serverError(errConCount, "Too many connections"))
	}

	files := replayRun(t, recording{}, goldenTarget("root"), refused)

	assertGoldens(t, files, "connect_failure", "my_metadata.txt", "my_sessions.txt")
}
