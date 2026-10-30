-- Roles and fixtures for the capture matrix (compose.mysql.yaml), run by the
-- image's entrypoint on first start.
--
-- Three roles, because the artifacts have three distinct privilege outcomes and
-- only a live server can tell them apart:
--
--   root           the superuser the image already creates: the "sees
--                  everything" baseline.
--
--   yc_monitor     PROCESS, REPLICATION CLIENT and SELECT on
--                  performance_schema - what the setup documentation
--                  recommends. Without PROCESS, performance_schema.threads
--                  shows only the role's own threads and
--                  information_schema.innodb_trx is refused.
--
--   yc_restricted  USAGE and nothing else - the least-privilege floor.
--
-- yc_replicator is not a matrix role: it exists for the replica profile, whose
-- replica connects to my84 with it.

CREATE USER 'yc_monitor'@'%' IDENTIFIED BY 'yc-monitor-pw';
GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'yc_monitor'@'%';
GRANT SELECT ON performance_schema.* TO 'yc_monitor'@'%';

CREATE USER 'yc_restricted'@'%' IDENTIFIED BY 'yc-restricted-pw';

CREATE USER 'yc_replicator'@'%' IDENTIFIED BY 'yc-replicator-pw';
GRANT REPLICATION SLAVE ON *.* TO 'yc_replicator'@'%';

-- The same table in two schemas, so the (schema_name, digest) key of
-- my_slow_queries.txt is tested: the same statement run in each schema is two
-- rows, never one merged row.
CREATE DATABASE yc_app;
CREATE DATABASE yc_app2;

CREATE TABLE yc_app.orders (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    customer_id INT NOT NULL,
    status      VARCHAR(16) NOT NULL,
    KEY orders_customer_idx (customer_id)
) ENGINE=InnoDB;

CREATE TABLE yc_app2.orders (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    customer_id INT NOT NULL,
    status      VARCHAR(16) NOT NULL,
    KEY orders_customer_idx (customer_id)
) ENGINE=InnoDB;

INSERT INTO yc_app.orders (customer_id, status)
    WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 500)
    SELECT i % 50, 'pending' FROM n;

INSERT INTO yc_app2.orders (customer_id, status)
    WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200)
    SELECT i % 20, 'shipped' FROM n;

-- No index at all, so a read of it is the index_name IS NULL row of
-- table_io_waits_summary_by_index_usage: a full scan.
CREATE TABLE yc_app.audit_log (
    at   DATETIME NOT NULL,
    note VARCHAR(64) NOT NULL
) ENGINE=InnoDB;

INSERT INTO yc_app.audit_log (at, note)
    WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300)
    SELECT '2026-10-27 00:00:00' + INTERVAL i SECOND, CONCAT('event ', i) FROM n;

-- The one table that is not InnoDB, for my_metadata.txt's share of InnoDB
-- tables: every other query in the MySQL section is blind to it.
CREATE TABLE yc_app.legacy_counters (
    name  VARCHAR(32) PRIMARY KEY,
    value BIGINT NOT NULL
) ENGINE=MyISAM;

INSERT INTO yc_app.legacy_counters VALUES ('visits', 42), ('signups', 7);

-- 120 more tables, so the LIMIT 100 of my_storage.txt and my_table_stats.txt
-- has something to cut.
CREATE DATABASE yc_bulk;

DELIMITER //

CREATE PROCEDURE yc_bulk.make_tables()
BEGIN
    DECLARE i INT DEFAULT 1;

    WHILE i <= 120 DO
        SET @ddl = CONCAT('CREATE TABLE yc_bulk.t', LPAD(i, 3, '0'),
                          ' (id INT PRIMARY KEY, v INT) ENGINE=InnoDB');
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;

        SET i = i + 1;
    END WHILE;
END //

DELIMITER ;

CALL yc_bulk.make_tables();
DROP PROCEDURE yc_bulk.make_tables;
