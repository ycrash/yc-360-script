# PostgreSQL capture — setup

A `postgres:` block under `options:` in the `-c` YAML file selects a database as
a capture target. There is no CLI flag: presence of the block is the switch, and
credentials never go on the command line because `ps -eLf` is captured into the
same bundle.

```yaml
options:
  postgres:
    host: db.internal
    port: 5432
    database: orders_db
    username: yc_monitor
    password: ${YC_PG_PASSWORD}
    sslmode: require
    captureDuration: 120s
    frequency: 30s
    explain: logged
```

`port`, `database`, `sslmode` and `captureDuration` may be omitted; they default
to `5432`, `postgres`, `require` and `120s`. `captureDuration` is capped at
`2h`.

### `frequency` — how often the periodic artifacts are sampled

```yaml
    frequency: 30s
```

It defaults to `5m`, which suits a long capture: a two-hour window samples
twenty-five times. It does not fit the default two-minute window, so a block that
sets neither key takes the opening and closing samples only, and the run warns
that it will. An incident capture that wants to see a blocking chain form and
clear sets `frequency: 30s`, as the example above does. A value below `10s` is
raised to `10s` with a warning, because a sample's statements are bounded at
`10s` and a faster cadence would let one slow sample outrun the tick behind it.
Whatever the value, the opening and closing samples are always taken.

### `agentOnDbHost` — only for a database that cannot answer

```yaml
    agentOnDbHost: true
```

The agent works out for itself whether it is running on the database's machine,
and captures host artifacts only when it establishes that it is (see *Where to
run it*). This key declares the deployment for the one case it cannot check: the
database is down, so there is no connection and no backend process to look for —
which is exactly when `dmesg` holds the kill and `df` holds the full disk.

It never overrides a measurement. A run that establishes the database is
elsewhere skips host capture anyway and logs the disagreement;
`pg_metadata.txt` records `agent_on_db_host_by=configured` whenever the
declaration, rather than a reading, is what authorised the capture. Omitting it
is the default, and setting it on a machine that is not the database host files
that machine's readings under the database's name.

### `explain` — the one key that is off by default

`explain` decides whether the bundle carries query plans, and **omitting it is
how you say no**. There is no `explain: off`; delete or comment the line.

| value | what runs | what it costs |
| --- | --- | --- |
| *(omitted)* | nothing — `pg_explain.txt` is one `reason=explain_disabled` block | — |
| `logged` | plans `auto_explain` already wrote to the server log, copied out | nothing is sent to the database; needs the agent on the database host |
| `all` | the above, plus plans the agent asks the server for | captured query text is prepared on your database and its plan asked for, on every supported version; with the server log readable and the prerequisites below met, the values your application bound are planned too |

Which queries get a plan is not a judgment the agent makes. Every distinct query
shape in `pg_stat_statements` is attempted once, in the first sample it is seen
in, and never again; the agent ranks nothing, and which shapes matter is the
server's call. A sample attempts at most ten, and the rest wait for the next
sample, so a database that walks in tracking thousands of shapes is explained as
a drip across the window rather than a burst at its start. Each block records
`first_seen=`, and each sample's summary says how many shapes still wait.

`all` is the only setting in this block that makes the agent *write* to your
database connection rather than read from it, which is why it is opt-in and why
configuring it prints a warning. `EXPLAIN` never executes the statement — the
plan-capture code has no `ANALYZE` path at all — but it does plan it, which
takes `AccessShareLock` on every relation involved. See the privileges and
sensitivity sections below before turning it on.

## A database capture is its own run

A configured block makes the run a database capture and nothing else. Passing an
application target alongside it is refused before anything is captured:

```
ERR  A postgres: block and an application target can not run together - the
     database capture is a separate run. Use a configuration file with no
     postgres: block for the application capture, or drop -p/-port and
     processTokens for the database capture.
```

**So the block does not go in the configuration file an existing deployment
already uses.** Give the database capture a file of its own:

```sh
./yc-360 -c app.yaml -p 4821        # application capture, no postgres: block
./yc-360 -c db.yaml                 # database capture, postgres: block, no PID
./yc-360 -c db.yaml -m3 -k <key>    # database monitoring, the same block on a loop
```

The one combination that is allowed is the third: `-m3` with a `postgres:` block
and **no** application target. That is database monitoring, and it is described
below. Every other combination is refused — `-p <pid>`, `-port <n>`, and `-m3`
with `processTokens` set.

One consequence worth knowing before you plan a rollout:

- **A mis-nested block is silent when an application target is present.** The
  block belongs under `options:`; at the top level of the file it decodes to
  nothing. On a database-only run that is loud — the run has no target left and
  stops with `nothing can be done`. Alongside `-p` it is not: the run has a
  target, so it proceeds and simply captures no database. Check the nesting.

## Monitoring a database on a loop

`-m3` with a `postgres:` block and no application target runs database
monitoring. Every cycle takes one small reading and sends it; the cycle length is
`m3Frequency`, three minutes by default. When the server asks for a capture, the
run uses the block's own `captureDuration` and `frequency`; the request carries
neither.

```sh
./yc-360 -c db-monitor.yaml -m3 -k <key>
```

The reading is a fresh connection and one statement. It carries how long the
connection took, how long the statement took, how many sessions are open against
the limit, and — when this machine runs the database — how much free space is
left on the database's own volumes. Nothing else. It computes no ratios and
applies no thresholds; the server does that.

Three things follow from that shape:

- **One connection per cycle, closed when the cycle ends.** A connection held
  open would occupy a `max_connections` slot permanently, on a database that may
  be out of them.
- **A value that could not be read is never a zero.** It is left out, and a
  reason row says why — `heartbeat_error` when the database did not answer,
  `disk_reason` when the volumes could not be read. A database that is down is
  the most important reading of all, so that payload is still sent.
- **Host data follows `agent_on_db_host`, not the presence of the block.** The
  CPU and memory capture describes the machine the agent runs on, so it is sent
  only where that machine is confirmed to be the database's. Where it is not,
  the reading carries the runner's load average and the agent's own CPU instead,
  so a slow heartbeat can be told from a struggling runner. See
  [Where to run it](#where-to-run-it).

The server may answer a cycle by asking for a full capture. When it does, the
agent runs exactly the deep dive `./yc-360 -c db.yaml` runs, and the next cycle
waits for it to finish.

Two limits to know:

- **`-onlyCapture` is ignored, as it is for every `-m3` run.** It is cleared at
  startup, with a warning that says so, and each cycle's reading uploads like the
  rest of the M3 stream. A database capture that has to stay local is the one-shot
  run: `./yc-360 -c db.yaml -onlyCapture` uploads nothing.
- **One runner, not N.** See [One sampler per cluster](#one-sampler-per-cluster);
  under `-m3` that rule is now the deployment model rather than a side effect of
  a refusal.

## Which database to name

**Name the application database. The default is `postgres`, and it costs you three
of the eleven artifacts — silently, with all three files reporting `status=complete`.**

`database:` is optional and defaults to `postgres`, which exists on effectively
every cluster. Most of the artifacts do not care which database you connect
through. Three of them care completely:

- `pg_bloat.txt` reads `pg_stat_user_tables`, which only ever shows the connected
  database's tables. Pointed at `postgres`, it captures a column header and no
  rows.
- `pg_index_usage.txt` reads `pg_stat_user_indexes`, the same view family, and
  shows the connected database's indexes only. Pointed at `postgres`, the same
  column header and no rows.
- `pg_slow_queries.txt` reads `pg_stat_statements`, and this one is the
  surprising case. The extension holds statistics for the **whole cluster**, but
  its view exists only in the databases where `CREATE EXTENSION` was actually
  run. So a capture connected to `postgres` while the extension lives in
  `orders_db` has nothing to read — no query statistics at all, on a cluster
  where they are all being collected.

The third one says so, in the file, rather than leaving you to work it out:

```
# ... source=pg_stat_statements ... library_loaded=true reason=extension_absent ...
```

`reason=` is one of four, and they have four different remedies:

| `reason=` | What happened | Fix |
| --- | --- | --- |
| `extension_absent` | No `pg_stat_statements` in this database | `CREATE EXTENSION pg_stat_statements` here, or point `database:` at the one that has it |
| `not_in_search_path` | Installed in a schema this session does not resolve — `extension_schema=` names it, `schema_usage=false` means the role lacks USAGE on it rather than the path being wrong | Add the schema to the role's `search_path`, or `GRANT USAGE` on it |
| `extension_too_old` | Below the 1.8 floor — `extension_version=` says which | `ALTER EXTENSION pg_stat_statements UPDATE` (one command, no restart) |
| `library_not_loaded` | The extension objects exist but the library is not in `shared_preload_libraries` | Add it and restart |

None of the four is an error: the capture worked, and what it found was a
configuration. `library_loaded=` is on the header of every capture regardless, so
a cluster with two of these problems at once shows both.

## Which role to use

Grant the capture role `pg_monitor`:

```sql
CREATE ROLE yc_monitor LOGIN PASSWORD '...';
GRANT pg_monitor TO yc_monitor;
```

The capture is read-only — it sets `default_transaction_read_only`,
`statement_timeout`, `lock_timeout` and `idle_in_transaction_session_timeout` on
its own session — so `pg_monitor` is the whole grant it needs.

### One exception: `explain: all`

`pg_monitor` does not let the agent plan a query. Under `explain: all` every
candidate comes back `error=permission denied for table …`, and that file is a
finding rather than an absence — the grant is the fix:

```sql
GRANT pg_read_all_data TO yc_monitor;
```

`pg_read_all_data` is a predefined role since PostgreSQL 14. It is broader than
this needs; the narrower alternative is per-table `GRANT SELECT` on the tables
you expect to appear.

**With the qualifier that matters:** `EXPLAIN` runs the executor's permission
checks, so this grant yields plans for SELECT-shaped candidates only. INSERT,
UPDATE, DELETE and MERGE candidates still return `permission denied` under
`pg_read_all_data` or any other read-only role. Those blocks are the expected
result, and the answer is **not** to grant write privileges to a monitoring
role — an unplanned write candidate is a smaller loss than a monitoring role
that can write.

`explain: all` also wants `database:` pointed at the application database. Under
the `postgres` default most candidates are counted `excluded_other_database`, and
where `pg_stat_statements` is absent there is nothing to attempt at all: the
summary says `statements_reason=extension_absent` rather than reporting an idle
database.

**Without `pg_monitor`, five artifacts lose data, and they lose it five
different ways.**

A role holding only `LOGIN` is *denied* some statements outright. Those failures
are loud: the block carries an `error=` header naming the refusal, so nothing is
silently missing.

`pg_replication.txt` is the first exception and it is worth understanding before
reading one. `pg_stat_replication` does not refuse a least-privilege role — it
returns the row and masks every column past `application_name` to NULL. So a
connected replica still appears, named, with **every lag and LSN column empty**:

```csv
pid,usesysid,usename,application_name,client_addr,...,write_lag_seconds,...
4021,16385,replicator,replica-01,,,,,,,,,,,,,,,,
```

That is not "the replica is caught up" — it is "nobody was allowed to look", and
an empty lag column is never a reading of zero. `pg_metadata.txt` records
`has_pg_monitor_role`, which is what tells the two apart. `pg_replication_slots`
needs no grant at all, so the WAL-retention half of that artifact is complete
either way.

`pg_sessions.txt` is the third case, and it is the quietest of the three — read
this one before trusting a least-privilege capture of it. `pg_stat_activity`
returns **every** row to any role, so the row count is right, the file looks
complete, and there is no `error=` anywhere. But on a session the role does not
own it masks twelve columns to NULL and replaces `query` with the literal string
`<insufficient privilege>`:

```csv
pid,datid,datname,...,state,...,backend_xid,backend_xmin,...,query
1093,16401,orders_db,...,,...,789,789,...,<insufficient privilege>
```

So a hundred sessions can appear to be running a statement by that name, every
`state` can read empty without a single session being idle, and nothing in the
file says why. What survives is the identity — which database, which role, which
application, and through `backend_xid` which session holds the transaction the
others are queued behind. `pg_locks` needs no grant at all, so the *shape* of a
blocking chain is still recoverable: who holds what, and who is queued behind
whom. What is lost is what any of those sessions was running.

`pg_slow_queries.txt` is the fourth case and it has the worst shape of all of
them: **it keeps every number and loses the key.**

`pg_stat_statements` returns every row to any role, and every counter in every
row is exact — `calls`, `total_exec_time`, `rows`, the block counters, all
correct. What a role without `pg_read_all_stats` does not get is `queryid`, which
reads NULL on every statement the role does not own, with `query` carrying the
same `<insufficient privilege>` sentinel:

```csv
queryid,userid,dbid,toplevel,...,calls,total_exec_time,...,query
,10,16401,true,...,128400,9820410.5,...,<insufficient privilege>
```

On a typical cluster that is most of the file. Measured on a matrix container:
277 of 319 rows.

The counters are real, so the artifact is not worthless — but `queryid` is the
key, and without it those rows cannot be matched from one sample to the
next, cannot be told apart from each other, and cannot be joined to
`pg_sessions.txt`'s `query_id`. The window delta the report is built from does
not exist for them. And nothing in the file says so: no `error=`, the right row
count, `status=complete`.

The one thing that reads normally is the statements the capture role executed
itself, which always keep their key — including the agent's own. Since NULLs sort
last, those rows also come first in the block.

The flag to read for both of the last two artifacts is `has_pg_read_all_stats`,
not `has_pg_monitor_role`: that is the gate the server actually applies, and a
role granted `pg_read_all_stats` directly sees everything while the monitor flag
says false. `pg_monitor` includes it, which is why the one grant above is still
the whole answer.

`pg_tablespaces.txt` is the fifth case and the smallest. `pg_tablespace_size`
refuses a tablespace the role holds no `CREATE` on, unless it is the database's
own default tablespace or the role has `pg_read_all_stats` — and an error raised
inside a select list aborts the whole statement, so the capture guards the call
with that same rule rather than letting one refusal cost every row. A size the
role may not read is an **empty cell, never `0`**, and the block's header counts
them as `sizes_unread=`. Under `LOGIN` alone that is every tablespace but the
default one; the artifact is still `status=complete`, and `has_pg_read_all_stats`
is again the flag that says why.

## What leaves the database

Read this before the first capture goes anywhere outside your perimeter.

`pg_slow_queries.txt`, `pg_sessions.txt` and `pg_explain.txt` carry **SQL
statement text**. In `pg_slow_queries.txt` that text is normalised for ordinary
queries — constants are replaced with `$1`, `$2` and so on, so values from your
data do not travel with it.

**`pg_sessions.txt` is not normalised.** It carries `pg_stat_activity.query` as
submitted, in every mode, literals included.

**Utility statements are the exception, and they are stored verbatim.** Under the
default `pg_stat_statements.track_utility = on`, DDL and other utility commands
are recorded exactly as submitted, literals included. Measured on PostgreSQL 18:

```
CREATE ROLE app_user LOGIN PASSWORD 'hunter2'   ← stored complete, with the password
ALTER ROLE app_user PASSWORD 'hunter2'          ← likewise
COPY t FROM PROGRAM 'some command'              ← likewise
```

So if anyone has ever run role DDL against a cluster, that cleartext is sitting
in `pg_stat_statements` and a capture will pick it up. Any role holding
`pg_read_all_stats` — which is to say the role this document recommends — can
read it.

**In Mode H the exposure is larger, and it is not normalised at all.**
`pg_deadlocks.txt` and `pg_timeouts.txt` copy the server's log verbatim, and a
deadlock's `DETAIL` reproduces each participant's statement **as submitted** —
literals included — as does every `STATEMENT:` line beside a timeout. On a real
application that is `UPDATE customers SET ssn = '…' WHERE email = '…'`.
`log_parameter_max_length` does **not** bound this, though it looks as though it
should: that setting bounds bind parameters logged with a statement, and the
text here is the statement. The agent cannot redact it and does not try — a
redacting agent is an agent parsing SQL. `MaxEventBytes` bounds the volume, not
the sensitivity, and `-onlyCapture` is the control that exists.

Three things follow:

- **The bundle inherits the exposure; the agent does not create it — with one
  exception, `explain: all`.** For every other artifact the value is in your
  database, put there by whoever ran the statement, and the agent captures the
  column as the server returns it. Under `explain: all` the agent *submits*
  statements it built from captured text — and, for the literal tier, the bind
  values the server logged — and a submission that errors can be written into
  your own server log by `log_min_error_statement` (default `error`), literals
  included. The exposure is small and your logging policy governs it, but the
  agent is a party to it there and nowhere else.
  `pg_metadata.txt` records `explain_mode=` and `explain_literals=verbatim` so
  the bundle says which.
- **`pg_metadata.txt` records whether the exposure is possible.** The
  `pg_stat_statements.track_utility` row says whether utility statements are
  tracked at all. Setting it to `off` closes it for future statements; the
  entries already recorded stay until they age out or the statistics are reset.
- **`-onlyCapture` keeps everything local — on a one-shot run.** It writes the
  bundle and uploads nothing, which is the mode to use while a security review is
  pending. Under `-m3` it is ignored and every cycle uploads; see
  [Monitoring a database on a loop](#monitoring-a-database-on-a-loop).

**Host artifacts are a separate exposure, and they are gated.** A confirmed
database host contributes its process list, connection table, kernel messages
and kernel settings — a command line can carry a credential, and a connection
table names every machine talking to this one. A run that cannot establish that
this machine is the database's captures none of them; see *Where to run it*.
`agentOnDbHost: true` is the one way to authorise that collection without a
measurement, which is why it warns at startup.

**How an `ESTIMATED_GENERIC` plan is made.** The agent prepares the normalized
query text under a name of its own, forces its session's `plan_cache_mode` to
`force_generic_plan`, asks for `EXPLAIN EXECUTE` with one `NULL` per parameter,
then resets the setting and deallocates the statement, on success and on
failure alike. The forced mode is what keeps the `NULL`s from selecting a custom
plan, so the plan keeps its `$1`, `$2` symbols; the block records how many
stood in as `parameters=`, and the plan's own `Settings:` line shows the forced
mode. This is one path on PostgreSQL 14 through 18; nothing is version-gated.
`EXPLAIN EXECUTE` without `ANALYZE` plans the statement and does not run it.

**How an `ESTIMATED_LITERAL` plan is made, and what it needs from you.** The
evidence is the server's own log. Under `log_min_duration_statement`, an
execution your application sent with bound parameters is logged as an `execute`
record whose `DETAIL` carries them: `Parameters: $1 = 'value', $2 = NULL`. The
agent parses that record — never splicing the log text into SQL — prepares the
record's own statement text under a name of its own, forces `plan_cache_mode` to
`force_custom_plan`, asks for `EXPLAIN EXECUTE` with each decoded value as a
literal, then resets and deallocates as above. The result is the server's custom
plan for the values that actually ran, and its `Query Identifier:` is the
statement's own, so `queryid_match=true` is expected. A block that fell to the
generic tier says why in `literal_reason=`. Three things have to be true on the
server side, none of which the agent will set for you:

- **`log_parameter_max_length` must be finite.** The tier runs only when the
  value the agent's connection observes is positive: `-1` (the default) means
  values are logged whole and the agent will not retain them
  (`literal_reason=parameter_cap_unbounded`); `0` means no parameters are logged
  at all (`parameters_not_logged`); an unreadable setting counts as unbounded
  (`parameter_cap_unread`). The agent never issues `SET log_parameter_max_length`
  or `ALTER SYSTEM`. Choose a cap, apply it to every backend whose statements you
  want planned, and prove it with a parameterized statement from the workload
  role: a value the server clipped at the cap comes back marked, and the agent
  refuses that record (`bind_record_truncated`) rather than planning a distorted
  value. The value in `pg_metadata.txt` is what the agent's own session saw,
  which is not proof for another role.
- **The log record must prove its query identifier.** csvlog and jsonlog carry
  it as a field; stderr carries it only if `log_line_prefix` contains `%Q`. A
  record with no identifier is counted (`binds_unidentified=` on the summary)
  and never attached — matching by query text is not a join.
- **`log_min_duration_statement` must be low enough** for the execution to be
  logged, and the agent must be able to read the log at all (`log_access=direct`).
  A capture that runs away from the database host gets generic plans only.

Each sample's summary counts what the log yielded (`binds_harvested=`, with
`binds_unidentified=`, `binds_rejected=` and `binds_dropped=` when non-zero), and
a block whose record was the slowest of several says `binds_seen=`.

**A limitation of the estimated plans, worth knowing before you read one.**
`pg_explain.txt`'s `ESTIMATED_LITERAL` and `ESTIMATED_GENERIC` blocks are plans
produced in the *agent's* session, not recreations of the application's. The
agent has its own `search_path`, its own role — so RLS policies apply to the
capture role, and per-role or per-database GUC overrides
(`ALTER ROLE app SET enable_seqscan = off`) never reach it — and no access to
another session's temporary objects. An unqualified name can resolve to a
different schema and yield a confidently wrong plan for the wrong table.

Three things in each block are the reader's tells: `VERBOSE` schema-qualifies
every relation the plan actually resolved to, `search_path=` records the agent's
own resolution context, and `plan_queryid=` with `queryid_match=false` is the
one machine-checkable symptom of a wrong resolution. `mode=LOGGED` blocks do not
have this problem at all — they are the server's own plan for the execution that
really happened, which is why that tier is tried first.

### What the bundle says about the connection itself

`pg_metadata.txt` records what the connection cost and how far apart the two
clocks are:

| row | what it is |
| --- | --- |
| `connect_ms` | how long establishing the connection took — TCP, TLS and authentication together, against the endpoint the run actually reached |
| `server_clock_timestamp` / `agent_ts_at_clock_read` | the server's clock and the agent's, read together; the difference is the skew between the two machines |
| `clock_read_rtt_ms` | the round trip of the query that read them, which is the error bar on that skew |

`ping.out` is not this measurement. It pings `pingHost` (`google.com` unless you
set it), which is a general internet-reachability check and says nothing about
the database — pointing it at the database instead would be worse, since managed
endpoints do not answer ICMP at all and would report 100% packet loss next to a
database report during an incident.

## Where to run it

Run it anywhere with network reachability to the database and you get every
artifact sourced from SQL — the only supported mode for managed PostgreSQL (RDS,
Aurora, Cloud SQL, Azure Database).

Run it on the database host and three more artifacts become available:
`pg_deadlocks.txt`, `pg_timeouts.txt` and `pg_checkpoint_log.txt`, which come
from the server's log file rather than from a query. The first two are the only
record in the bundle of what the database *did to a transaction* — the
participants of a deadlock, and which statement a timeout killed — and neither
needs any logging configuration: `log_min_messages = warning` and
`deadlock_timeout = 1000` are the defaults, so every default installation logs
all four events. The third copies each checkpoint's completion line, with the
buffers written and the `write=`, `sync=` and `total=` costs the counters in
`pg_capacity.txt` cannot give, and it does need one setting:
`log_checkpoints = on`, the default since PostgreSQL 15 and off on 14.
`pg_metadata.txt` records the value, so an empty file can be read against it.

Host artifacts (`top`, `ps`, `vmstat`, `netstat`, `dmesg`, `df`, `kernel`)
describe the machine that ran the script, so a database capture takes them only
when the run establishes that this is the database's machine. It does that by
looking up the backend process the server reported for its own connection —
`pg_metadata.txt` records the answer in `agent_on_db_host`, the test behind it,
and, whenever the answer is not `yes`, the reason. `host_artifacts` says what
the run did with it, so a bundle with no host files explains itself, and the
agent log carries the deployment change that would turn most reasons into a
`yes`.

The same answer gates database monitoring's CPU and memory stream: under `-m3`
the `top` capture runs only on a confirmed database host, and where it does not,
the cycle's reading carries `runner_load1` and `agent_cpu_pct` in its place. So
host data in the monitoring stream exists exactly where `agent_on_db_host=yes`,
with no label to interpret.

Two things follow. A run against a managed service or a remote host never
uploads a foreign machine's process list and connection table under the
database's name. And on a database host, `netstat` and `ps` stretch their
readings across `captureDuration` — `netstat` at both edges, `ps` spread evenly
over three — so a connection table or process list that changes during the
window leaves a trace. That holds for the longest window too: on a two-hour
capture `netstat`'s two readings are two hours apart and `ps`'s an hour. The other four are unchanged: `top` and `vmstat` take
their own fixed ~20 s burst at the opening edge, and `df`, `dmesg` and `kernel`
take one reading, exactly as an application capture takes them. Every one of
these files uploads under the same identifier whichever kind of capture wrote
it, so their formats are shared and none of them changes here.

## `log_access` is a permission, not a location

**"On the database host" is not enough, and a default installation denies it.**
Measured on PostgreSQL 14 through 18: the data directory is `0700`, the log
directory inside it is `0700`, every log file is `0600`, and
`log_file_mode = 0600`. A dedicated service account reads none of it, so an agent
sitting on the database host reports `log_access=none` and all three log
artifacts say `reason=unreadable` with the path they could not open. That is why the field
is named for the experiment it runs — opening the file the server named — and not
for a location it never tested.

That is the outcome you hit first. Three deployments make Mode H actually work:

1. **Run the agent as the `postgres` OS user.** Works everywhere and needs no
   configuration change. It is also the largest footprint for a binary that
   uploads what it reads.
2. **Move `log_directory` outside the data directory**, set
   `log_file_mode = 0640`, and put the agent's account in the `postgres` group.
   This is the recommended footprint. Both halves are needed — without moving the
   directory, the `0700` above it denies everything regardless of the file mode —
   and `log_file_mode` needs a reload and applies only to files created after it,
   so either wait for a rotation or force one with `SELECT pg_rotate_logfile()`.
   Group membership is snapshotted at session start, so start the agent from a
   fresh login.
3. **A group-accessible data directory** — `initdb --allow-group-access`, `0750`
   — plus `log_file_mode = 0640`. Only available if the cluster was initialised
   that way; it cannot be applied to a running cluster.

**And a fourth reality that is none of the three: the Debian family does not run
the logging collector at all.** Debian and Ubuntu packaging ships
`logging_collector = off` and redirects the server's stderr through the cluster
wrapper to `/var/log/postgresql/postgresql-NN-main.log` — a file PostgreSQL
cannot name, so the agent refuses to guess at it. Both artifacts report
`reason=collector_off` in every deployment until the collector is enabled. The
PGDG RPM packaging and the official containers run the collector, and are
deployments 1–3 territory.

Whichever you get, the artifact says which: every block carries `log_access=`
(`direct`, `none` or `unknown`) and `log_resolved_by=`, and where there is no log
to read it carries a `reason=` — `collector_off`, `unresolved`, `unreadable` or
`settings_unread` — and **no
`matched=` key at all**. That absence is deliberate. `matched=0` means the log
was read and held no event; a `reason=` means there was nothing to read, and
there is no zero for a report to render as "no deadlocks occurred".

Two more keys carry the same distinction inside a block that *did* read
something. `scan_truncated=true skipped_bytes=<n>` says the log outgrew what one
sample reads, so an event may have occurred in the gap. `resolved_late=true`
says this block's `from_offset` is where the artifact's coverage *begins* rather
than where the last one ended — the log was not readable at the start of the
window, and what was written before the tail got a handle was never read. Both
mean the same thing to a report: the `matched=` beside them counts a shorter
window than the preamble's `window=`.

`pg_health.txt`'s `pg_stat_database.deadlocks` counter is a fallback for
deadlocks — a count, without participants. **Nothing anywhere counts timeouts**,
so `pg_timeouts.txt` is the one artifact in the feature with no substitute.

## One sampler per cluster

One database capture per run, and one runner per database. Nothing enforces
this: N hosts each invoking a capture against the same database give that
database N concurrent samplers during an incident, which is when it can least
afford them. Nominate one runner — the database host, or one host that can reach
it — and keep the `postgres:` block in that host's configuration file and
nowhere else.

Sharing one configuration file across N hosts running `-m3` is the way this
usually happens by accident, and since database monitoring runs on `-m3` there is
no longer a refusal standing in its way: N runners with that file give the
database N pollers, every cycle, for as long as they run. Each reading names its
runner and its target, so the server can see the duplication — but the agent
cannot see the other runners and does not try to. Keep the block on one host.
