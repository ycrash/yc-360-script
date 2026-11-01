package mysql

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// globalStatusColumns are SHOW GLOBAL STATUS/VARIABLES columns.
var globalStatusColumns = []string{"Variable_name", "Value"}

// showGlobal is SHOW GLOBAL STATUS or VARIABLES for the named variables. Always
// GLOBAL: the unqualified form reads the agent's own session.
func showGlobal(kind string, names ...string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "'" + name + "'"
	}

	return "SHOW GLOBAL " + kind + " WHERE Variable_name IN (" + strings.Join(quoted, ", ") + ")"
}

// NewCapacity is my_capacity.txt: connection and throughput counters.
// Threads_connected includes the agent's own connections (spec §1.3).
func NewCapacity(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_capacity", ConnectionNormal, interval),
		blocks: func(SampleContext) []block {
			return []block{{
				source: "global_status",
				query: showGlobal("STATUS", "Threads_connected", "Threads_running", "Aborted_connects",
					"Questions", "Threads_cached", "Threads_created", "Connections"),
				columns: globalStatusColumns,
			}}
		},
	}
}

// maxThreadMemoryRows bounds my_memory.txt's per-connection block.
const maxThreadMemoryRows = 1000

// NewMemory is my_memory.txt: buffer pool, connection and engine memory. Summary
// blocks read zeros with memory/% instruments off (has_memory_instruments_enabled).
func NewMemory(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_memory", ConnectionNormal, interval),
		blocks: func(SampleContext) []block {
			return []block{
				{
					source:  "buffer_pool_variables",
					query:   showGlobal("VARIABLES", "innodb_buffer_pool_size", "innodb_page_size"),
					columns: globalStatusColumns,
				},
				{
					source: "buffer_pool_status",
					query: showGlobal("STATUS", "Innodb_buffer_pool_pages_total", "Innodb_buffer_pool_pages_free",
						"Innodb_buffer_pool_bytes_data"),
					columns: globalStatusColumns,
				},
				{
					// Instance-wide table counts; per-digest is in my_slow_queries.txt.
					source:  "temp_spill_status",
					query:   showGlobal("STATUS", "Created_tmp_disk_tables", "Sort_merge_passes"),
					columns: globalStatusColumns,
				},
				{
					source: "memory_by_thread",
					query: limitSQL(`SELECT t.THREAD_ID, t.PROCESSLIST_ID, t.PROCESSLIST_USER, t.PROCESSLIST_HOST,
       t.PROCESSLIST_DB, t.PROCESSLIST_COMMAND,
       SUM(m.CURRENT_NUMBER_OF_BYTES_USED) AS current_bytes
FROM performance_schema.memory_summary_by_thread_by_event_name m
JOIN performance_schema.threads t ON m.THREAD_ID = t.THREAD_ID
WHERE t.PROCESSLIST_ID IS NOT NULL
GROUP BY t.THREAD_ID, t.PROCESSLIST_ID, t.PROCESSLIST_USER, t.PROCESSLIST_HOST, t.PROCESSLIST_DB,
         t.PROCESSLIST_COMMAND
ORDER BY current_bytes DESC`, maxThreadMemoryRows),
					columns: []string{"THREAD_ID", "PROCESSLIST_ID", "PROCESSLIST_USER", "PROCESSLIST_HOST",
						"PROCESSLIST_DB", "PROCESSLIST_COMMAND", "current_bytes"},
					maxRows:      maxThreadMemoryRows,
					instrumented: true,
				},
				{
					source: "prepared_statement_memory",
					query: `SELECT EVENT_NAME, CURRENT_NUMBER_OF_BYTES_USED, HIGH_NUMBER_OF_BYTES_USED
FROM performance_schema.memory_summary_global_by_event_name
WHERE EVENT_NAME IN ('memory/sql/Prepared_statement::main_mem_root',
                     'memory/sql/Prepared_statement::infrastructure')`,
					columns:      []string{"EVENT_NAME", "CURRENT_NUMBER_OF_BYTES_USED", "HIGH_NUMBER_OF_BYTES_USED"},
					instrumented: true,
				},
				{
					// Excludes the buffer pool, counted above.
					source: "innodb_memory",
					query: `SELECT EVENT_NAME, CURRENT_NUMBER_OF_BYTES_USED
FROM performance_schema.memory_summary_global_by_event_name
WHERE EVENT_NAME LIKE 'memory/innodb/%' AND EVENT_NAME != 'memory/innodb/buf_buf_pool'`,
					columns:      []string{"EVENT_NAME", "CURRENT_NUMBER_OF_BYTES_USED"},
					instrumented: true,
				},
				{
					source: "performance_schema_memory",
					query: `SELECT SUM(CURRENT_NUMBER_OF_BYTES_USED) AS current_bytes
FROM performance_schema.memory_summary_global_by_event_name
WHERE EVENT_NAME LIKE 'memory/performance\_schema/%'`,
					columns:      []string{"current_bytes"},
					instrumented: true,
				},
			}
		},
	}
}

// NewNonDefaultSettings is my_nondefault_settings.txt: every variable not at its
// compiled default, joined to its global value (empty for session-only ones).
func NewNonDefaultSettings(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_nondefault_settings", ConnectionExpensive, interval),
		blocks: func(SampleContext) []block {
			return []block{{
				source: "variables_info",
				query: `SELECT i.VARIABLE_NAME, i.VARIABLE_SOURCE, i.VARIABLE_PATH, g.VARIABLE_VALUE,
       i.SET_TIME, i.SET_USER, i.SET_HOST
FROM performance_schema.variables_info i
LEFT JOIN performance_schema.global_variables g ON g.VARIABLE_NAME = i.VARIABLE_NAME
WHERE i.VARIABLE_SOURCE != 'COMPILED'
ORDER BY i.VARIABLE_NAME`,
				columns: []string{"VARIABLE_NAME", "VARIABLE_SOURCE", "VARIABLE_PATH", "VARIABLE_VALUE",
					"SET_TIME", "SET_USER", "SET_HOST"},
			}}
		},
	}
}

// NewErrors is my_errors.txt: instance-wide error counts, including the agent's
// own refusals (each asked once).
func NewErrors(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_errors", ConnectionNormal, interval),
		blocks: func(SampleContext) []block {
			return []block{{
				source: "events_errors_summary_global_by_error",
				query: `SELECT error_number, error_name, sql_state, sum_error_raised, sum_error_handled,
       first_seen, last_seen
FROM performance_schema.events_errors_summary_global_by_error
WHERE sum_error_raised > 0
ORDER BY error_number`,
				columns: []string{"error_number", "error_name", "sql_state", "sum_error_raised",
					"sum_error_handled", "first_seen", "last_seen"},
				instrumented: true,
			}}
		},
	}
}

// timeoutErrors: lock wait timeout, max_execution_time, idle session closed (8.0.24+).
var timeoutErrors = []string{"1205", "3024", "4031"}

// NewTimeouts is my_timeouts.txt: timeout error counts and Aborted_clients. Not
// read from the error log as the spec says: the server doesn't log these (measured).
func NewTimeouts(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_timeouts", ConnectionNormal, interval),
		blocks: func(SampleContext) []block {
			return []block{
				{
					source: "events_errors_summary_global_by_error",
					query: `SELECT error_number, error_name, sum_error_raised, first_seen, last_seen
FROM performance_schema.events_errors_summary_global_by_error
WHERE error_number IN (` + strings.Join(timeoutErrors, ", ") + `)
ORDER BY error_number`,
					columns:      []string{"error_number", "error_name", "sum_error_raised", "first_seen", "last_seen"},
					instrumented: true,
				},
				{
					source:  "global_status",
					query:   showGlobal("STATUS", "Aborted_clients"),
					columns: globalStatusColumns,
				},
			}
		},
	}
}

// bufferPoolColumns are SHOW ENGINE INNODB STATUS columns.
var bufferPoolColumns = []string{"Type", "Name", "Status"}

// BufferPool is my_buffer_pool.txt: SHOW ENGINE INNODB STATUS's report as text,
// redacted.
type BufferPool struct {
	Interval time.Duration

	state blockState
}

func (b *BufferPool) Artifact() Artifact {
	artifact := periodicArtifact("my_buffer_pool", ConnectionNormal, b.Interval)
	artifact.Format = dbcore.FormatText

	return artifact
}

func (b *BufferPool) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	const source = "innodb_status"

	artifact := b.Artifact()
	statement := block{source: source, query: "SHOW ENGINE INNODB STATUS", columns: bufferPoolColumns}

	if b.state.isRefused(source) {
		return writeTextBlock(w, artifact, s, statement, dbcore.Span{}, dbcore.StatusOK,
			[]dbcore.HeaderField{kv("reason", reasonPermissionDenied)}, nil)
	}

	res, err := read(ctx, q, s, block{query: statement.query})

	switch {
	case err != nil && permissionDenied(err):
		b.state.refuse(source)

		return writeTextBlock(w, artifact, s, statement, res.reads, dbcore.StatusOK,
			[]dbcore.HeaderField{kv("reason", reasonPermissionDenied), kv("error", s.ErrorText(err))}, nil)

	case err != nil:
		return err
	}

	var report string
	if len(res.rows) > 0 && len(res.rows[0]) == len(bufferPoolColumns) {
		report = res.rows[0][2]
	}

	body, redacted := redactMonitorText(report)

	return writeTextBlock(w, artifact, s, statement, res.reads, dbcore.StatusOK,
		[]dbcore.HeaderField{kv("redacted", strconv.Itoa(redacted))}, []byte(body))
}

// writeTextBlock writes a header with bytes= and the body, newline-terminated.
func writeTextBlock(w io.Writer, artifact Artifact, s SampleContext, b block, reads dbcore.Span, status string,
	fields []dbcore.HeaderField, body []byte,
) error {
	if len(body) > 0 && body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}

	rows := 0
	if len(body) > 0 {
		rows = 1
	}

	var buf bytes.Buffer

	if err := dbcore.WriteSampleHeader(&buf, engineName, location(s), artifact, s, dbcore.SampleHeader{
		Source: b.source,
		Reads:  reads,
		Status: status,
		Rows:   rows,
		Fields: append(fields, kv("bytes", strconv.Itoa(len(body)))),
	}); err != nil {
		return err
	}

	buf.Write(body)

	_, err := w.Write(buf.Bytes())

	return err
}
