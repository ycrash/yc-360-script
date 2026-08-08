-- Roles and extensions for the capture matrix (compose.pg.yaml), run by the
-- postgres image's entrypoint against POSTGRES_DB on first start.
--
-- Three roles, because the artifact has three distinct privilege outcomes and
-- only a live server can tell them apart:
--
--   postgres       the superuser the image already creates: the "sees
--                  everything" baseline.
--
--   yc_monitor     GRANT pg_monitor - what the setup documentation will
--                  recommend. Still denied pg_current_logfile() on 14-16: the
--                  EXECUTE grant to pg_monitor landed in 17.
--
--   yc_restricted  LOGIN and nothing else - the least-privilege floor, which
--                  decides whether pg_settings omits the superuser-only
--                  settings (a complete artifact) or raises (no artifact).

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

CREATE ROLE yc_monitor LOGIN PASSWORD 'yc-monitor-pw';
GRANT pg_monitor TO yc_monitor;

CREATE ROLE yc_restricted LOGIN PASSWORD 'yc-restricted-pw';

-- Tables for pg_bloat.txt. autovacuum is disabled so the dead tuples below
-- survive to the assertions.
--
-- Deliberately no ANALYZE: n_live_tup and n_dead_tup are maintained
-- incrementally from the DML reports, and ANALYZE overwrites them with its own
-- estimate. Measured - with ANALYZE here, a table holding 400 rows reported
-- n_live_tup=800 on every server in the matrix.

-- Indexed, and with dead tuples: 500 inserted, 100 deleted.
CREATE TABLE yc_bloat_orders (id bigserial PRIMARY KEY, status text);
ALTER TABLE yc_bloat_orders SET (autovacuum_enabled = false);
INSERT INTO yc_bloat_orders (status) SELECT 'x' FROM generate_series(1, 500);
DELETE FROM yc_bloat_orders WHERE id <= 100;

-- No indexes at all: idx_scan is NULL here and 0 on the table above, which is
-- the empty-versus-zero distinction the artifact turns on.
CREATE TABLE yc_bloat_no_indexes (id bigint, note text);
ALTER TABLE yc_bloat_no_indexes SET (autovacuum_enabled = false);
INSERT INTO yc_bloat_no_indexes SELECT g, 'n' FROM generate_series(1, 250) g;

-- A partitioned parent and two partitions: three more rows in
-- pg_stat_user_tables, and the only relkind in that view whose relations have
-- no storage of their own. It is what proves the size functions neither raise
-- nor need a grant on the partitioned schemas the cap exists for, and that they
-- report such a relation as 0 rather than NULL.
CREATE TABLE yc_bloat_parted (id bigint, at date) PARTITION BY RANGE (at);
CREATE TABLE yc_bloat_parted_p1 PARTITION OF yc_bloat_parted
    FOR VALUES FROM ('2026-01-01') TO ('2026-07-01');
CREATE TABLE yc_bloat_parted_p2 PARTITION OF yc_bloat_parted
    FOR VALUES FROM ('2026-07-01') TO ('2027-01-01');

-- 200 more tables, so a lowered MaxTables has something to truncate. On every
-- server rather than on one: compose.pg.yaml's invariant is that every
-- container gets the same init script.
CREATE SCHEMA yc_bulk;

DO $$
BEGIN
    FOR i IN 1..200 LOOP
        EXECUTE format('CREATE TABLE yc_bulk.t%s (id int)', lpad(i::text, 3, '0'));
    END LOOP;
END
$$;
