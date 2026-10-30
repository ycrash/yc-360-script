package mysql

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactStatementKeepsTheStatementAndReplacesItsValues(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     string
		redacted int
	}{
		{"strings and numbers",
			"SELECT * FROM orders WHERE customer_id = 42 AND status = 'pending'",
			"SELECT * FROM orders WHERE customer_id = <redacted> AND status = '<redacted>'", 2},
		{"a double-quoted string, as the default sql_mode reads it",
			`UPDATE t SET note = "it's here" WHERE id = 7`,
			`UPDATE t SET note = "<redacted>" WHERE id = <redacted>`, 2},
		{"backtick names stay, digits in them too",
			"SELECT `order 1`.`total` FROM `order 1` WHERE `x2` = 3.5e10",
			"SELECT `order 1`.`total` FROM `order 1` WHERE `x2` = <redacted>", 1},
		{"escapes inside a literal",
			`SELECT 'O\'Brien', 'it''s', 'tab\\'`,
			`SELECT '<redacted>', '<redacted>', '<redacted>'`, 3},
		{"prefixed literals keep their prefix",
			"SELECT X'4D59', b'101', N'naïve', _utf8mb4'abc', _binary'\x01'",
			"SELECT X'<redacted>', b'<redacted>', N'<redacted>', _utf8mb4'<redacted>', _binary'<redacted>'", 5},
		{"hex and bit numbers",
			"SELECT 0xDEADBEEF, 0b1010, .5, 1e-3",
			"SELECT <redacted>, <redacted>, <redacted>, <redacted>", 4},
		{"a name that starts with a digit is a name",
			"SELECT 1st_quarter FROM t1 WHERE c = 1",
			"SELECT 1st_quarter FROM t1 WHERE c = <redacted>", 1},
		{"keywords, placeholders and variables stay",
			"SELECT NULL, TRUE, ?, @v, @@session.sql_mode FROM t WHERE a IS NOT NULL",
			"SELECT NULL, TRUE, ?, @v, @@session.sql_mode FROM t WHERE a IS NOT NULL", 0},
		{"plain comments and hints stay",
			"SELECT /*+ MAX_EXECUTION_TIME(1000) */ a /* note 5 */ FROM t # trailing 'x'\nWHERE b = 2 -- it's\n",
			"SELECT /*+ MAX_EXECUTION_TIME(1000) */ a /* note 5 */ FROM t # trailing 'x'\nWHERE b = <redacted> -- it's\n", 1},
		{"-- without a space is two minuses",
			"SELECT 5--3",
			"SELECT <redacted>--<redacted>", 2},
		{"an executable comment's body is SQL",
			"SELECT /*!80000 'secret', 9 */ a FROM t",
			"SELECT /*!80000 '<redacted>', <redacted> */ a FROM t", 2},
		{"a literal cut by the server's text limit is replaced to the end",
			"INSERT INTO t VALUES ('first', 'sec",
			"INSERT INTO t VALUES ('<redacted>', '<redacted>", 2},
		{"date literals",
			"SELECT * FROM t WHERE at > DATE '2026-10-27' - INTERVAL 1 DAY",
			"SELECT * FROM t WHERE at > DATE '<redacted>' - INTERVAL <redacted> DAY", 2},
		{"the agent's own marker stays",
			"/* yCrash-DB-Agent */ SELECT 1",
			"/* yCrash-DB-Agent */ SELECT <redacted>", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n := redactStatement(tt.in)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.redacted, n)
		})
	}
}

func TestRedactStatementReplacesACredentialStatementWhole(t *testing.T) {
	tests := []struct{ in, want string }{
		{"CREATE USER 'app'@'%' IDENTIFIED BY 's3cret'", "CREATE USER <redacted>"},
		{"alter user app identified with caching_sha2_password as '$A$005$x'", "alter user <redacted>"},
		{"  /* admin */ SET PASSWORD FOR 'app'@'%' = 'n3w'", "  /* admin */ SET PASSWORD <redacted>"},
		{"CHANGE REPLICATION SOURCE TO SOURCE_PASSWORD='p'", "CHANGE REPLICATION <redacted>"},
		{"START REPLICA USER='r' PASSWORD='p'", "START REPLICA <redacted>"},
		{"CREATE SERVER s FOREIGN DATA WRAPPER mysql OPTIONS (PASSWORD 'p')", "CREATE SERVER <redacted>"},
	}

	for _, tt := range tests {
		got, n := redactStatement(tt.in)
		assert.Equal(t, tt.want, got)
		assert.Equal(t, 1, n)
	}

	got, _ := redactStatement("SELECT user FROM mysql.user WHERE user = 'create user'")
	assert.Equal(t, "SELECT user FROM mysql.user WHERE user = '<redacted>'", got,
		"the words count only as the statement's own")
}

func TestRedactMonitorTextKeepsInnoDBsCountersAndReplacesTheRows(t *testing.T) {
	report := `TRANSACTION 2575, ACTIVE 3 sec starting index read
mysql tables in use 1, locked 1
LOCK WAIT 3 lock struct(s), heap size 1128, 2 row lock(s), undo log entries 1
MySQL thread id 169, OS thread handle 281472159428352, query id 318 127.0.0.1 root updating
UPDATE yc_app.orders SET status='a' WHERE id=2
RECORD LOCKS space id 2 page no 6 n bits 464 index PRIMARY of table ` + "`yc_app`.`orders`" + ` trx id 2575 lock_mode X locks rec but not gap
Record lock, heap no 2 PHYSICAL RECORD: n_fields 5; compact format; info bits 0
 0: len 8; hex 8000000000000001; asc         ;;
 4: len 1; hex 61; asc a;;
 5: SQL NULL;
`

	got, n := redactMonitorText(report)

	assert.Equal(t, `TRANSACTION 2575, ACTIVE 3 sec starting index read
mysql tables in use 1, locked 1
LOCK WAIT 3 lock struct(s), heap size 1128, 2 row lock(s), undo log entries 1
MySQL thread id 169, OS thread handle 281472159428352, query id 318 127.0.0.1 root updating
UPDATE yc_app.orders SET status='<redacted>' WHERE id=<redacted>
RECORD LOCKS space id 2 page no 6 n bits 464 index PRIMARY of table `+"`yc_app`.`orders`"+` trx id 2575 lock_mode X locks rec but not gap
Record lock, heap no 2 PHYSICAL RECORD: n_fields 5; compact format; info bits 0
 0: len 8; hex <redacted>; asc <redacted>;;
 4: len 1; hex <redacted>; asc <redacted>;;
 5: SQL NULL;
`, got)
	assert.Equal(t, 4, n)
}

func TestRedactMonitorTextReadsAMultiLineStatementToTheNextStructure(t *testing.T) {
	status := `---TRANSACTION 421947, ACTIVE 3 sec
MySQL thread id 12, OS thread handle 140, query id 45 localhost root executing
SELECT *
FROM orders WHERE email = 'a@b.c'
------- TRX HAS BEEN WAITING 2 SEC FOR THIS LOCK TO BE GRANTED:
---TRANSACTION 421948, not started
MySQL thread id 13, OS thread handle 141, query id 46 localhost root
---TRANSACTION 421949, ACTIVE 1 sec`

	got, n := redactMonitorText(status)

	assert.Contains(t, got, "FROM orders WHERE email = '<redacted>'\n------- TRX HAS BEEN WAITING 2 SEC")
	assert.Contains(t, got, "---TRANSACTION 421948, not started\nMySQL thread id 13, OS thread handle 141")
	assert.Equal(t, 1, n)
}
