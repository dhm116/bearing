-- The durable event log (contracts.EventLog). Every statement is idempotent.

-- One row per partition. head is the latest offset handed out and trimmed the
-- highest offset Trim removed; both survive a Trim of every event.
CREATE TABLE IF NOT EXISTS event_head (
    partition VARCHAR PRIMARY KEY,
    head      BIGINT NOT NULL,
    trimmed   BIGINT NOT NULL
);

-- The events. Offsets start at 1 per partition and may have gaps after Trim.
-- The id is unique across partitions because it starts with its partition.
CREATE TABLE IF NOT EXISTS event_log (
    partition   VARCHAR NOT NULL,
    off         BIGINT NOT NULL,
    id          VARCHAR NOT NULL UNIQUE,
    type        VARCHAR NOT NULL,
    time        TIMESTAMPTZ NOT NULL,
    data        BLOB NOT NULL,
    retain      BOOLEAN NOT NULL,
    appended_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (partition, off)
);

-- The offset each consumer group has processed in each partition.
CREATE TABLE IF NOT EXISTS event_commit (
    grp       VARCHAR NOT NULL,
    partition VARCHAR NOT NULL,
    off       BIGINT NOT NULL,
    PRIMARY KEY (grp, partition)
);
