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
