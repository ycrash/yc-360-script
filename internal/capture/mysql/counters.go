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

// Status and variable reads: the counters and settings each sample reads whole.

// globalStatusColumns are SHOW GLOBAL STATUS's and SHOW GLOBAL VARIABLES's own.
var globalStatusColumns = []string{"Variable_name", "Value"}

// showGlobal is SHOW GLOBAL STATUS or VARIABLES for the named variables, in one
// statement. GLOBAL, never the unqualified form: that is the agent's own session.
// A name the server does not have is absent from the result.
func showGlobal(kind string, names ...string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "'" + name + "'"
	}

	return "SHOW GLOBAL " + kind + " WHERE Variable_name IN (" + strings.Join(quoted, ", ") + ")"
}

// NewCapacity is my_capacity.txt: connection and throughput counters.
// Threads_connected includes the agent's own connections, a reference value only
// (spec §1.3): the report counts connections from my_sessions.txt. Connections is
// read beside Threads_created for the thread cache's hit rate.
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

// NewMemory is my_memory.txt: the Buffer Pool, Connections and Internal Engine
// Memory regions, one block per statement. The memory-summary blocks read zeros
// where the memory/% instruments are off; my_metadata.txt's
// has_memory_instruments_enabled says which.
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
					// A count of tables, never bytes, and instance-wide: per-statement
					// attribution is my_slow_queries.txt's sum_created_tmp_disk_tables.
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
					maxRows: maxThreadMemoryRows,
				},
				{
					source: "prepared_statement_memory",
					query: `SELECT EVENT_NAME, CURRENT_NUMBER_OF_BYTES_USED, HIGH_NUMBER_OF_BYTES_USED
FROM performance_schema.memory_summary_global_by_event_name
WHERE EVENT_NAME IN ('memory/sql/Prepared_statement::main_mem_root',
                     'memory/sql/Prepared_statement::infrastructure')`,
					columns: []string{"EVENT_NAME", "CURRENT_NUMBER_OF_BYTES_USED", "HIGH_NUMBER_OF_BYTES_USED"},
				},
				{
					// The buffer pool's own instrument is excluded, so the two regions
					// never count it twice.
					source: "innodb_memory",
					query: `SELECT EVENT_NAME, CURRENT_NUMBER_OF_BYTES_USED
FROM performance_schema.memory_summary_global_by_event_name
WHERE EVENT_NAME LIKE 'memory/innodb/%' AND EVENT_NAME != 'memory/innodb/buf_buf_pool'`,
					columns: []string{"EVENT_NAME", "CURRENT_NUMBER_OF_BYTES_USED"},
				},
				{
					source: "performance_schema_memory",
					query: `SELECT SUM(CURRENT_NUMBER_OF_BYTES_USED) AS current_bytes
FROM performance_schema.memory_summary_global_by_event_name
WHERE EVENT_NAME LIKE 'memory/performance\_schema/%'`,
					columns: []string{"current_bytes"},
				},
			}
		},
	}
}

// NewNonDefaultSettings is my_nondefault_settings.txt: every variable not at its
// compiled-in default, with its current value. The spec's text joins
// performance_schema.global_variables for the value and its query did not; this is
// the joined query. Both tables are readable without a grant. A session-only
// variable has no global value, and keeps an empty one.
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

// NewErrors is my_errors.txt: the server's own error counts, aggregated across the
// instance. A refusal the agent itself met is counted here too, once per artifact
// that met it: a refused read is asked once and remembered.
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
			}}
		},
	}
}

// timeoutErrors are the errors a timeout ends a statement or a session with:
// lock wait timeout, max_execution_time, and a session closed for inactivity
// (8.0.24 and later; older servers end it without a word to the client).
var timeoutErrors = []string{"1205", "3024", "4031"}

// NewTimeouts is my_timeouts.txt. The spec reads it from the error log, but the
// server writes none of these there: a lock wait timeout (1205) and an execution
// timeout (3024) go to the client and nowhere else, and an idle session's end is a
// Note only log_error_verbosity=3 writes, measured on the matrix. The errors are
// counted instead, by number, beside Aborted_clients - every session that ended
// without closing, wait_timeout's among them.
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
					columns: []string{"error_number", "error_name", "sum_error_raised", "first_seen", "last_seen"},
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

// bufferPoolColumns are SHOW ENGINE INNODB STATUS's.
var bufferPoolColumns = []string{"Type", "Name", "Status"}

// BufferPool is my_buffer_pool.txt: SHOW ENGINE INNODB STATUS, written as text. Its
// one value is the monitor's whole report, whose line breaks the server's parser
// reads by, so the body is the report itself rather than a CSV cell. Its
// TRANSACTIONS and LATEST DETECTED DEADLOCK sections quote statements and print
// rows; both have their values replaced.
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

// writeTextBlock writes a text block: its header, with bytes= the body's length,
// then the body, ending in a newline, so the next header starts a line.
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
