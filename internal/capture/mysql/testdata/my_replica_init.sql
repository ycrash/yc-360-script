-- The replica profile's replica of my84 (compose.mysql.yaml), run by the
-- image's entrypoint on first start. GTID auto-positioning, so the replica
-- starts from the source's first transaction and replays the matrix init with
-- the rest.
--
-- GET_SOURCE_PUBLIC_KEY: the replication channel runs in plaintext between two
-- containers on a private network, and caching_sha2_password needs the key for
-- a plaintext first login. Test fixture only - the agent never sets its own
-- equivalent (see docs/mysql-capture.md).

CHANGE REPLICATION SOURCE TO
    SOURCE_HOST = 'my84',
    SOURCE_PORT = 3306,
    SOURCE_USER = 'yc_replicator',
    SOURCE_PASSWORD = 'yc-replicator-pw',
    SOURCE_AUTO_POSITION = 1,
    GET_SOURCE_PUBLIC_KEY = 1;

START REPLICA;
