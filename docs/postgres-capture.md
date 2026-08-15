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
```

`port`, `database`, `sslmode` and `captureDuration` may be omitted; they default
to `5432`, `postgres`, `require` and `120s`. `captureDuration` is capped at
`600s`.

## A database capture is its own run

A configured block makes the run a database capture and nothing else. Passing an
application target alongside it is refused before anything is captured:

```
ERR  A postgres: block and an application target can not run together - the
     database capture is a separate run. Use a configuration file with no
     postgres: block for the application capture, or drop -p/-m3/-port for the
     database capture.
```

That applies to all three: `-p <pid>`, `-m3`, and `-port <n>`.

**So the block does not go in the configuration file an existing deployment
already uses.** Give the database capture a file of its own:

```sh
./yc-360 -c app.yaml -p 4821        # application capture, no postgres: block
./yc-360 -c db.yaml                 # database capture, postgres: block, no PID
```

Two consequences worth knowing before you plan a rollout:

- **There is no recurring database capture.** `-m3` is one of the refused
  targets, so a database cannot be sampled every incident cycle today.
- **A mis-nested block is silent when an application target is present.** The
  block belongs under `options:`; at the top level of the file it decodes to
  nothing. On a database-only run that is loud — the run has no target left and
  stops with `nothing can be done`. Alongside `-p` it is not: the run has a
  target, so it proceeds and simply captures no database. Check the nesting.

## Which database to name

**Name the application database. The default is `postgres`, and it costs you two
of the seven artifacts — silently, with both files reporting `status=complete`.**

`database:` is optional and defaults to `postgres`, which exists on effectively
every cluster. Most of the artifacts do not care which database you connect
through. Two of them care completely:

- `pg_bloat.txt` reads `pg_stat_user_tables`, which only ever shows the connected
  database's tables. Pointed at `postgres`, it captures a column header and no
  rows.
- `pg_slow_queries.txt` reads `pg_stat_statements`, and this one is the
  surprising case. The extension holds statistics for the **whole cluster**, but
  its view exists only in the databases where `CREATE EXTENSION` was actually
  run. So a capture connected to `postgres` while the extension lives in
  `orders_db` has nothing to read — no query statistics at all, on a cluster
  where they are all being collected.

The second one says so, in the file, rather than leaving you to work it out:

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

**Without `pg_monitor`, four artifacts lose data, and they lose it four
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
key, and without it those rows cannot be matched between the start and end
samples, cannot be told apart from each other, and cannot be joined to
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

## What leaves the database

Read this before the first capture goes anywhere outside your perimeter.

`pg_slow_queries.txt` and `pg_sessions.txt` carry **SQL statement text**. For
ordinary queries that text is normalised — constants are replaced with `$1`,
`$2` and so on, so values from your data do not travel with it.

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

- **The bundle inherits the exposure; the agent does not create it.** The value
  is in your database, put there by whoever ran the statement, and the agent
  captures the column as the server returns it.
- **`pg_metadata.txt` records whether the exposure is possible.** The
  `pg_stat_statements.track_utility` row says whether utility statements are
  tracked at all. Setting it to `off` closes it for future statements; the
  entries already recorded stay until they age out or the statistics are reset.
- **`-onlyCapture` keeps everything local.** It writes the bundle and uploads
  nothing, which is the mode to use while a security review is pending.

## Where to run it

Run it anywhere with network reachability to the database and you get every
artifact sourced from SQL — the only supported mode for managed PostgreSQL (RDS,
Aurora, Cloud SQL, Azure Database).

Run it on the database host and two more artifacts become available:
`pg_deadlocks.txt` and `pg_timeouts.txt`, which come from the server's log file
rather than from a query. They are the only record in the bundle of what the
database *did to a transaction* — the participants of a deadlock, and which
statement a timeout killed — and neither needs any logging configuration:
`log_min_messages = warning` and `deadlock_timeout = 1000` are the defaults, so
every default installation logs all four events.

Host artifacts (`top`, `ps`, `vmstat`, `netstat`, `dmesg`, `df`) always describe
the machine that ran the script, which is the database host only in the second
mode.

## Mode H is a permission, not a location

**"On the database host" is not enough, and a default installation denies it.**
Measured on PostgreSQL 14 through 18: the data directory is `0700`, the log
directory inside it is `0700`, every log file is `0600`, and
`log_file_mode = 0600`. A dedicated service account reads none of it, so an agent
sitting on the database host reports `capture_mode=pg-remote` and both log
artifacts say `reason=unreadable` with the path they could not open.

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

Whichever you get, the artifact says which: every block carries `capture_mode=`
and `log_resolved_by=`, and where there is no log to read it carries a `reason=`
— `collector_off`, `unresolved`, `unreadable` or `mode_unknown` — and **no
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
usually happens by accident. Today that particular mistake cannot reach the
database, because a block alongside `-m3` is refused outright (above) — but the
refusal is a side effect, not a guard, so the rule stands on its own.
