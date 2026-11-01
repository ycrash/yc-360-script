//go:build mysqlintegration

package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"yc-agent/internal/capture/dbcore"
)

// Run with YC_MY_RECORD=1 (rewrites testdata), then YC_UPDATE_GOLDEN=1 go test -run TestGolden.
func TestRecordGoldens(t *testing.T) {
	if os.Getenv("YC_MY_RECORD") == "" {
		t.Skip("YC_MY_RECORD=1 rewrites testdata/my84_*.json")
	}

	server := matrixServers[2]
	root := rootDB(t, server)
	app := rootDBOn(t, server, "yc_app")

	// Replaced in the recording so a replay is machine-independent.
	var client string
	require.NoError(t, root.QueryRow("SELECT SUBSTRING_INDEX(USER(), '@', -1)").Scan(&client))

	sanitise := strings.NewReplacer(client, "192.0.2.10", "/tmp/yc-mylogs/", "/yc-recording/logs/")

	stageBeforeWindow(t, root, app)

	record(t, server, matrixRoles[0], "my84_root.json", sanitise, func() {
		var wg sync.WaitGroup

		wg.Go(func() { stageDuringWindow(t, root) })
		wg.Go(func() { forceDeadlock(t, root) })
		wg.Wait()
	})

	record(t, server, matrixRoles[2], "my84_restricted.json", sanitise, nil)
}

func record(t *testing.T, server matrixServer, role matrixRole, name string, sanitise *strings.Replacer, during func()) {
	t.Helper()

	recorder := &recorder{}
	target := matrixTarget(server, role)

	dial := func(ctx context.Context) (Querier, dbcore.Closer, error) {
		conn, err := Connect(ctx, target)
		if err != nil {
			return nil, nil, err
		}

		return recorder.wrap(conn), conn, nil
	}

	dir := t.TempDir()
	t.Chdir(dir)

	window := &Window{
		Engine:     engine(target, dial),
		Duration:   goldenWindow,
		Collectors: goldenCollectors(target, goldenStart),
		CaptureID:  goldenCaptureID,
		TargetID:   target.id(),
		Database:   target.Database,
	}

	var wg sync.WaitGroup

	if during != nil {
		wg.Go(during)
	}

	for _, result := range window.Run(context.Background()) {
		if result.File != nil {
			result.File.Close()
		}

		require.Equal(t, StatusComplete, result.Status, "%s: %s", result.Artifact.FileName, result.Err)
	}

	wg.Wait()

	r := recording{Server: server.service, Role: role.user, Statements: recorder.statements}
	sanitiseRecording(&r, sanitise)

	content, err := json.MarshalIndent(r, "", " ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(packageDir, "testdata", name), append(content, '\n'), 0o644))
}

func sanitiseRecording(r *recording, sanitise *strings.Replacer) {
	for i := range r.Statements {
		statement := &r.Statements[i]

		if statement.Error != nil {
			statement.Error.Message = sanitise.Replace(statement.Error.Message)
		}

		for _, row := range statement.Rows {
			for j, value := range row {
				if value != nil {
					replaced := sanitise.Replace(*value)
					row[j] = &replaced
				}
			}
		}
	}
}

type recorder struct {
	mu         sync.Mutex
	statements []recordedStatement
}

func (r *recorder) wrap(q Querier) Querier { return &recordingQuerier{q: q, recorder: r} }

func (r *recorder) keep(statement recordedStatement) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.statements = append(r.statements, statement)
}

type recordingQuerier struct {
	q        Querier
	recorder *recorder
}

func (q *recordingQuerier) Query(ctx context.Context, query string) (Rows, error) {
	statement := recordedStatement{Query: query}

	rows, err := q.q.Query(ctx, query)
	if err == nil {
		statement.Columns, statement.Rows, err = readRecorded(rows)
	}

	if err != nil {
		statement.Error = &recordedError{Message: err.Error()}

		var serverErr *gomysql.MySQLError
		if errors.As(err, &serverErr) {
			statement.Error = &recordedError{Number: serverErr.Number, Message: serverErr.Message}
		}

		q.recorder.keep(statement)

		return nil, err
	}

	q.recorder.keep(statement)

	return &replayRows{columns: statement.Columns, rows: statement.Rows}, nil
}

func readRecorded(rows Rows) ([]string, [][]*string, error) {
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}

	var all [][]*string

	values := make([]sql.NullString, len(columns))
	dest := make([]any, len(columns))

	for i := range values {
		dest[i] = &values[i]
	}

	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}

		row := make([]*string, len(columns))

		for i, value := range values {
			if value.Valid {
				text := value.String
				row[i] = &text
			}
		}

		all = append(all, row)
	}

	return columns, all, rows.Err()
}
