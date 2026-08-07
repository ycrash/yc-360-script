# `pg_metadata.txt` golden fixtures

The two `.txt` files here are the byte-for-byte contract for `pg_metadata.txt`,
one per regime (`plan_pgmetadata.md` §5). `pg_matrix_init.sql` is not a
fixture — it is the role and extension setup `compose.pg.yaml` mounts into each
container for the opt-in version matrix (`-tags pgintegration`), and it lives
here because it is test input the package owns.

The goldens:

- `pg_metadata_full.txt` — a complete capture. PostgreSQL 17 deliberately:
  `pg_monitor` was not granted EXECUTE on `pg_current_logfile()` until 17, so
  a 14–16 fixture showing `has_pg_monitor_role,true` next to
  `capture_mode,pg-dbhost` would depict a deployment that needs an extra
  manual grant (`pg_metadata_impl_notes.txt` §2a).
- `pg_metadata_connect_failure.txt` — a run that never reached the server. A
  non-empty `connect_error` is the discriminator: the file stops there.

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

## Editing

The files are LF-terminated with a single trailing newline; `.gitattributes`
pins that against eol rewriting. `log_line_prefix`'s value ends in a
significant trailing space — `TestGoldenKeepsTrailingWhitespace` guards it
against trimming editors.

To change a fixture, change the writer or the `Metadata` samples in
`writer_test.go` and argue the resulting diff — never hand-edit these files,
and keep `plan_pgmetadata.md` §5 in step with them.
