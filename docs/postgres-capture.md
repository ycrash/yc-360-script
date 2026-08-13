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

`database:` is optional and defaults to `postgres`, which exists on effectively
every cluster. The cluster-wide artifacts do not care which database you connect
through. Naming the application database is what enables the database-scoped
ones — `pg_bloat.txt` reads `pg_stat_user_tables`, which only ever shows the
connected database's tables.

## Which role to use

Grant the capture role `pg_monitor`:

```sql
CREATE ROLE yc_monitor LOGIN PASSWORD '...';
GRANT pg_monitor TO yc_monitor;
```

The capture is read-only — it sets `default_transaction_read_only`,
`statement_timeout`, `lock_timeout` and `idle_in_transaction_session_timeout` on
its own session — so `pg_monitor` is the whole grant it needs.

**Without `pg_monitor`, three artifacts lose data, and they lose it three
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

The flag to read here is `has_pg_read_all_stats`, not `has_pg_monitor_role`:
that is the gate the server actually applies, and a role granted
`pg_read_all_stats` directly sees everything while the monitor flag says false.
`pg_monitor` includes it, which is why the one grant above is still the whole
answer.

## Where to run it

Run it anywhere with network reachability to the database and you get every
artifact sourced from SQL — the only supported mode for managed PostgreSQL (RDS,
Aurora, Cloud SQL, Azure Database).

Run it on the database host and the log-derived artifacts become available too,
since the agent can read the log directory. "On the database host" does not mean
"as the `postgres` OS user": a service account with read access to the log
directory is the right footprint.

Host artifacts (`top`, `ps`, `vmstat`, `netstat`, `dmesg`, `df`) always describe
the machine that ran the script, which is the database host only in the second
mode.

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
