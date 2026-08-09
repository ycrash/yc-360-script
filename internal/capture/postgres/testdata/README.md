# Postgres artifact golden fixtures

The `.txt` files here are the byte-for-byte contract for the artifacts this
package writes, one per regime.
`pg_matrix_init.sql` is not a fixture — it is the role and extension setup
`compose.pg.yaml` mounts into each container for the opt-in version matrix
(`-tags pgintegration`), and it lives here because it is test input the package
owns.

The goldens:

- `pg_metadata_full.txt` — a complete capture. PostgreSQL 17 deliberately:
  `pg_monitor` was not granted EXECUTE on `pg_current_logfile()` until 17, so
  a 14–16 fixture showing `has_pg_monitor_role,true` next to
  `capture_mode,pg-dbhost` would depict a deployment that needs an extra
  manual grant (`pg_metadata_impl_notes.txt` §2a).
- `pg_metadata_connect_failure.txt` — a run that never reached the server. A
  non-empty `connect_error` is the discriminator: the file stops there.
- `pg_bloat_full.txt` — a complete sampled capture: the preamble, two sample
  blocks, and the closing block that says both were written.
- `pg_bloat_connect_failure.txt` — the sampled equivalent of the above. Two
  lines, and the file exists at all because the preamble is written and synced
  before the connection is attempted.
- `pg_bloat_sample_error.txt` — one sample's statement timed out. The window
  writes the stub block carrying `sample_error=`, the window does not stop, and
  the closing block says `status=partial`.
- `pg_bloat_empty_db.txt` — a complete capture of a database with no user
  tables. A column header with no rows: captured and found nothing, which is a
  different shape from could not be captured.
- `pg_health_full.txt` — a complete interval capture, on a 30s window so three
  samples fit on a page; the default 120s window is the same shape with twelve.
  `pg_stat_database` is read **unfiltered**, so the block carries every database
  in the cluster plus the `datid=0` shared-objects row, whose `datname` is NULL
  because it accounts for shared relations rather than a database.
  `scope=cluster`, and `db=`/`dbid=` mean *connected through*, not *about*.
- `pg_health_connect_failure.txt` — two lines, as above.
- `pg_health_sample_error.txt` — sample 2's statement timed out. Sample 3 is
  still taken: a failed sample never ends the window.
- `pg_health_no_sessions_fatal.txt` — a server predating `sessions_fatal`
  (PostgreSQL 13 and older). The sample is retried without the column, the
  header carries `sessions_fatal=unavailable` with the quoted `reason=`, and
  the column stays in the body with every cell empty — a key present in one
  block of a shape is present in every block of that shape. `status=complete`:
  ten of eleven columns is a captured sample, not a failed one.

## Reader requirements

They apply to every consumer, including the server-side parser. Verified
empirically against `pg_metadata_full.txt`.

- Parse CSV **records**, not lines. The block header is a single-field record;
  every other record has exactly two fields.
- Allow variable field counts. In Go: `reader.FieldsPerRecord = -1`. The
  default (`0`) locks onto the one-field block header and rejects `key,value`
  at line 2.
- Do **not** treat `#` as a comment character (in Go: leave `Comment` unset).
  That configuration parses "cleanly" and silently discards the block header
  carrying `engine=`, `v=`, `format=` and `scope=`.
- The writer flattens every value onto one line (CR/LF become spaces), so one
  record is one line and only line 1 begins with `#`.

### Block header keys

Split the header on unquoted whitespace into `k=v` tokens. `engine`, `source`,
`v`, `format` and `scope` always lead and `ts` always closes, so a block's
identity and its clock read are readable without parsing the middle.

- A key written with an empty value means "not read" — `dbid=` before a
  connection exists. It is not the same as the key being absent.
- Header keys, unlike body keys, may be **conditional**: `sizes=`, `reason=`
  and `connect_error=` appear only when the thing they describe happened, and
  in each case absence is itself the value.
- A sampled artifact's preamble carries `schedule=` (`start_end` or `every`)
  and `interval=` (`10s`, or empty on `start_end`). Both are written for every
  artifact on every schedule, so the preamble is one fixed key set:
  `samples_expected` does not explain itself without the cadence, and the
  cadence is the agent's constant rather than the server's.
- **Deltas are computed against each block's own `ts=`, never against
  `interval=`.** A sample runs late rather than being skipped, so two blocks
  with near-identical `ts=` mean the sampler was catching up. `interval=` is
  the nominal cadence, there to be compared against `ts=`.

### Versioning

`v=` is the artifact format version. The rule, stated once so it need not be
inferred from a diff: **additive keys do not bump `v`; removed or re-formed
keys do.** A reader that tokenises `k=v` is not got wrong by a key it has never
seen, and is got wrong by one that has changed meaning or gone away. `v=1`
therefore survived `schedule=` and `interval=` arriving.

## Editing

The files are LF-terminated with a single trailing newline; `.gitattributes`
pins that against eol rewriting. `log_line_prefix`'s value ends in a
significant trailing space — `TestGoldenKeepsTrailingWhitespace` guards it
against trimming editors.

To change a fixture, change the writer or the samples in `writer_test.go`,
`bloat_test.go` and `health_test.go`, and argue the resulting diff — never
hand-edit these files.
