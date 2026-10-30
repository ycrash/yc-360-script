# MySQL capture matrix — spike results

`my_matrix_init.sql` and `my_replica_init.sql` are not fixtures: they are the
roles, tables and replication setup `compose.mysql.yaml` mounts into each
container for the opt-in matrix (`-tags mysqlintegration`), and they live here
because they are test input the package owns.

This file is step 1 of `mysql-capture-plan.md`'s order of work: what the spec's
queries do on real servers, recorded so the code that depends on them does not
rest on belief. Measured 2026-10-27 on the two 8.0 servers, and 2026-10-30 on
all five.

## What was run

| Server | Image | Version |
|---|---|---|
| `my80` | `mysql:8.0` | 8.0.46 |
| `my80old` | `mysql:8.0.27` (amd64, emulated on Apple Silicon) | 8.0.27 |
| `my84` | `mysql:8.4` | 8.4.13 |
| `my9` | `mysql:9` | 9.7.4 |
| `percona84` | `percona/percona-server:8.4` | 8.4.11-11 |

`go test -tags mysqlintegration` passes on all five, every role:
`TestMatrixWindow` (every file, all three roles), `TestMatrixScenarios` (the
staged cases below), `TestMatrixPlaintextLoginNeverRequestsTheServersKey`, and
`TestMatrixVerifiedTLS` on `my84`. A finding marked *8.0 only* was probed by
hand on the two 8.0 servers; the matrix asserts the parts of it that the
recommended roles depend on everywhere.

Not run: the `replica` and `perms` profiles, and a MariaDB server.

## The plan's *(verify)* statements

- **The 8.0.27 image is amd64-only.** Confirmed: compose needs
  `platform: linux/amd64` for it on Apple Silicon. It runs under emulation.
- **`UMASK: "0644"` makes `error.log` readable from the host.** Confirmed: the
  file is `-rw-r--r--`. The data directory's own files are unaffected.
- **Cancelling a context closes the socket.** Confirmed in the driver's source
  (`watchCancel`): the connection is marked closed, and `Conn.Lost()` reports
  it, so a context deadline mid-statement ends that connection's timeline - the
  same behaviour pgx has, and the reason `StatementDeadline` sits above
  `max_execution_time`.
- **Public key retrieval.** The plan assumed an `allowPublicKeyRetrieval`
  switch to leave off. go-sql-driver has none: over plaintext it always asks the
  server for its RSA key when `caching_sha2_password` or `sha256_password` wants
  full authentication. The agent therefore guards the plaintext socket and
  refuses that one packet (`keyRequestGuard`), failing the login with
  `connect_error=public_key_retrieval_refused`. Measured: after `FLUSH
  PRIVILEGES` the plaintext login is refused; right after a TLS login by the
  same account it succeeds, because the server caches the hash - the
  intermittent failure the plan predicted. A wrong password over plaintext also
  ends in the key request, since the server asks for full authentication
  whenever its quick check fails, and the advice says so.
- **The auto-generated certificate has no subject alternative name.**
  Confirmed on `percona84`: "No extensions in certificate", subject
  `CN=MySQL_Server_8.4.11-11_Auto_Generated_Server_Certificate`. Go's verifier
  cannot accept it, so verified TLS needs a real certificate; `my84` serves
  the setup script's, which names `127.0.0.1` and `localhost`, and verifies.
  A `serverName` the certificate does not carry is refused.
- **The public key guard on every version.** After `FLUSH PRIVILEGES`, a
  plaintext login is refused before the key request on all five servers.
- **MariaDB lacks the tables the spec relies on.** Not measured; the variant is
  detected from `VERSION()` and recorded.

## Privileges, per role (8.0 only)

| Read | `root` | `yc_monitor` | `yc_restricted` |
|---|---|---|---|
| `performance_schema.*` | yes | yes | 1142, except below |
| `performance_schema.global_variables`, `variables_info` | yes | yes | **yes**: readable with no grant |
| `information_schema.innodb_trx` | yes | yes | 1227 (PROCESS) |
| `SHOW ENGINE INNODB STATUS` | yes | yes | 1227 (PROCESS) |
| `SHOW REPLICA STATUS`, `SHOW MASTER STATUS` | yes | yes | 1227 (REPLICATION CLIENT) |
| `information_schema.TABLES`, application schemas | all | **none** | none |
| `SHOW GLOBAL STATUS`, `@@log_error` | yes | yes | yes |

- **`information_schema.TABLES` lists only tables the role holds a privilege
  on.** `yc_monitor`, the recommended role, sees none of the application's
  tables: `my_storage.txt` is empty for it, and `my_metadata.txt`'s
  `tables_visible` is 0. The setup documentation recommends a grant on the
  application schemas for that reason.
- **A configured `database:` the role may not use refuses the connection**
  (1044), and the window records `connect_failed` on every file without
  dialling again: it is the target's failure, not a slot's.
- **The agent's own refusals are counted by the server.** Each refused read
  adds one to `events_errors_summary_global_by_error` under 1142 or 1227, which
  `my_errors.txt` reads. So a refused block is asked once per capture and
  remembered (`reason=permission_denied` on every later sample, with no
  statement run), rather than adding one every sample.
- `information_schema.USER_PRIVILEGES` lists only the account's direct grants,
  and every account's rows for a superuser: `my_metadata.txt` filters to
  `CURRENT_USER()`. A privilege held through a role is not listed;
  `enabled_roles` says when that may be the case.

## Capability flags

Asserted by `TestMatrixWindow` for the two roles that may read
`performance_schema`.

| Flag | `my80` | `my80old` | `my84` | `my9` | `percona84` |
|---|---|---|---|---|---|
| `has_cpu_time_instrument_enabled` | false, `consumers_off` | false, `version_before_8.0.28` | true | true | false, `consumers_off` |
| `has_statement_memory_tracking` | true | false | true | true | true |
| `has_memory_instruments_enabled` | false (`memory/%=OFF`) | true | true | true | true |

- `--performance-schema-instrument=memory/%=OFF` leaves the 76
  `memory/performance_schema/%` instruments on: they cannot be switched off.
  `has_memory_instruments_enabled` counts every other `memory/%` instrument.
  With them off, the memory summaries read zeros, not NULLs.
- On 8.0.27 the `events_statements_cpu` consumer does not exist, and
  `sum_cpu_time`, `max_total_memory` and `max_controlled_memory` are not
  columns: `my_slow_queries.txt` selects `NULL` in their place and names them
  as `unavailable=`.
- With the consumer on (`my84`, `my9`), `sum_cpu_time` is read and nothing is
  named `unavailable=`.

## The spec questions (plan §6)

1. **`my_timeouts.txt`'s source.** Settled: error 1205 (lock wait timeout) and
   3024 (`max_execution_time` exceeded) are returned to the client and written
   nowhere in the error log, at verbosity 2 or 3. Both are counted in
   `events_errors_summary_global_by_error`. `my_timeouts.txt` therefore reads
   those counters (1205, 3024 and 4031) and `Aborted_clients`, at normal
   speed, instead of tailing the log. Not measured: the "Aborted connection"
   note an idle session's end writes at verbosity 3.
2. **`performance_schema.error_log` as a deadlock fallback.** Settled, no: at
   verbosity 2 it receives nothing from a deadlock; at verbosity 3 it receives
   the `[Note]` framing lines (`MY-012468` and `MY-012469`) but none of the
   statements or record dumps, which InnoDB writes past the log sink. It can
   count deadlocks, not describe them. The fallback stays `SHOW ENGINE INNODB
   STATUS`'s LATEST DETECTED DEADLOCK, which `my_buffer_pool.txt` already
   captures.
3. **`hasMysqlErrorLogAccess`.** Defined as Postgres defines log access:
   `@@log_error` resolved as the server would (absolute, or relative to
   `@@datadir`), then opened by this process. `stderr` - the official image's
   default - is `log_access=none reason=stderr`.
   **The deadlock report has two shapes**, and the tail reads both: at
   verbosity 3, a `[Note] [MY-012468]` line and `[MY-012469]` section
   headings around InnoDB's dump; at the default verbosity of 2 the notes are
   filtered and the dump alone remains, starting at a bare `TRANSACTION <id>,
   ACTIVE` line with no `deadlock` text anywhere. The dump prints each locked
   row as hex and text (` 4: len 1; hex 61; asc a;;`), which the redaction
   replaces.
4. **`information_schema_stats_expiry`.** 86400 by default, so
   `my_storage.txt`'s sizes may be a day old. Left alone: setting it to 0 opens
   every table. `my_metadata.txt` records the value.
5. **Replication statements by version.** `SHOW REPLICA STATUS` from 8.0.22,
   `SHOW SLAVE STATUS` before; `SHOW BINARY LOG STATUS` from 8.2, `SHOW MASTER
   STATUS` before. On 8.0.46 `SHOW MASTER STATUS` still works; on 8.4.13, 9.7.4
   and Percona 8.4.11 it is a syntax error (1064), and `SHOW BINARY LOG STATUS`
   answers.
6. **`my_nondefault_settings.txt`.** The joined query is written:
   `variables_info` LEFT JOIN `global_variables`, so a session-only variable
   keeps an empty value.
7. **`my_metadata.txt`'s flags.** All written; see the table above.
8. **Same-host detection.** MySQL is one process, so the test is the server's
   own start: a local `mysqld` or `mariadbd` whose start time is within 2s of
   `now - Uptime`. On this machine Docker runs the servers in a VM, and the
   verdict is `no reason=no_server_process`, which is right. The yes path is
   unit-tested only.

## Staged cases (`TestMatrixScenarios`, all five)

Before the window, the scenario truncates the digest and index-usage summaries
(test setup only: the agent never writes to the server) and runs a slow
application `SELECT` and twenty scans of the table with no index. During it, a
transaction holds a row with `FOR UPDATE`; a second session waits for that row;
an `ALTER TABLE` queues for the metadata lock behind the transaction; a session
holds a server-side prepared statement with a literal in it; and a statement
runs past `max_execution_time`. Every file is `status=complete`, and:

- `my_lock_waits.txt`: a fast sample has `data_lock_waits` rows and a pending
  `metadata_locks` row naming `yc_app.orders`.
- `my_prepared_statements.txt`: the held statement, `status = '<redacted>'`,
  its `?` kept, and none of the agent's own.
- `my_timeouts.txt`: 1205 rises by at least two between the opening and closing
  samples (the row lock and the metadata lock both end in it) and 3024 by at
  least one.
- `my_explain.txt`: the application `SELECT` is planned under
  `schema_name=yc_app`, with no value of it left in the plan.
- `my_table_stats.txt`: the scanned table's row has an empty `INDEX_NAME`.

## Other findings

- **`EXPLAIN` of an `UPDATE` or `DELETE` fails in a read-only session**: error
  1792, "Cannot execute statement in a READ ONLY transaction". The session
  keeps `transaction_read_only=1`, and `my_explain.txt` skips write statements
  with `reason=read_only_session`.
- **9.x writes a second JSON plan format, and it carries the values
  elsewhere.** 9.7.4's `EXPLAIN FORMAT=JSON` is `json_schema_version` 2.0: the
  whole rewritten statement in `query`, and each step's expression in
  `operation` ("Filter: (orders.`status` = 'pending')") and `lookup_condition`
  ("customer_id = 7"). The first version of the plan redaction replaced values
  only under a list of condition keys and left these in place; the matrix
  caught it. Plan redaction is now deny-by-default: every string has its
  values replaced unless its key holds only names, structure or costs.
- **A prepared statement's sample keeps its `?` markers**, and EXPLAIN of it
  fails with 1064, which spent a plan on nothing. Such a sample is skipped as
  `reason=placeholder`.
- **EXPLAIN of a `FOR UPDATE` does not wait on a locked row**: it returned at
  once with the row held. The session's `innodb_lock_wait_timeout` is still set
  to 2s, against the server's 50s default, as a bound on what is not known.
- **9.7.4 reports `gtid_mode=ON`** with no option setting it.
- **A statement that failed has no sample.** `query_sample_text` is empty and
  `query_sample_seen` is `0000-00-00 00:00:00.000000` for a digest whose every
  run failed (the 3024 test's `SELECT SLEEP`).
- **`schema_name` is empty** for statements run with no default database. The
  digest key is still `(schema_name, digest)`.
- 8.0.46 logs a `[Warning] [MY-013360]` about `sha256_password`'s deprecation
  on every login by the image's own `mysql` client. Whether the agent's logins
  add the same line was not separated out.
- The two compose files shared one project name, the directory's, so each
  file's `up` and `down` saw the other's containers as orphans.
  `compose.mysql.yaml` now names its project `yc-mysql-matrix`.
