#!/bin/bash
# Runs the storage-engine benchmark on ClickHouse. Usage:
#   clickhouse.sh CLICKHOUSE_BINARY DATA_DIR
# Talks to a server on localhost:9000.
set -e
CH=$1 D=$2
C="$CH client --port 9000"
tm() { local s; s=$(date +%s.%N); "$@"; printf "  %.2fs\n" "$(echo "$(date +%s.%N) - $s" | bc)"; }

$C -q "DROP TABLE IF EXISTS version"
$C -q "CREATE TABLE version (tbl LowCardinality(String), key String, n Int32, rec Int64, ret Int64, data String)
  ENGINE = MergeTree ORDER BY (tbl, key, n) SETTINGS enable_block_number_column = 1, enable_block_offset_column = 1"
echo "seed 1M rows:"; tm $C -q "INSERT INTO version FORMAT TSV" < "$D/seed.tsv"
$C -q "OPTIMIZE TABLE version FINAL"
echo "apply 50,000 new rows (one INSERT, atomic as one part):"; tm $C -q "INSERT INTO version FORMAT TSV" < "$D/apply.tsv"
echo "supersede 5,000 live rows, mutation (ALTER UPDATE, synchronous):"
tm $C -q "ALTER TABLE version UPDATE ret = 1800000000000000 WHERE n = 4 AND key IN (SELECT key FROM version WHERE n = 4 AND ret = 0 LIMIT 5000) SETTINGS mutations_sync = 1"
echo "supersede 5,000 live rows, lightweight UPDATE:"
tm $C -q "UPDATE version SET ret = 1800000000000001 WHERE n = 4 AND key IN (SELECT key FROM version WHERE n = 4 AND ret = 0 LIMIT 5000) SETTINGS allow_experimental_lightweight_update = 1"
$C -q "OPTIMIZE TABLE version FINAL"
# clickhouse benchmark takes no parameters, so the keys are literals: a
# computed key would stop the primary index from being used.
python3 -I -c "
import random; random.seed(2)
for _ in range(2000):
    print(f\"SELECT n, rec, ret, data FROM version WHERE tbl = 'fact' AND key = 'acme/k{4 * random.randrange(50000) + 2}' AND rec <= 1767226600000000 AND (ret = 0 OR ret > 1767226600000000)\")
" > "$D/ch-point.sql"
echo "point read of one series as of a record time (10 s each):"
for c in 1 16; do $CH benchmark --port 9000 -c $c -t 10 < "$D/ch-point.sql" 2>&1 | grep QPS | tail -1 | sed "s/^/  c=$c /"; done
echo "load two series with key IN (...), surrealstore's load shape:"
$C --time -q "SELECT tbl, key, n, rec, ret, data FROM version WHERE rec <= 1800000000000000 AND tbl = 'fact' AND key IN ('acme/k2', 'acme/k6') ORDER BY n FORMAT Null" 2>&1 | sed 's/^/  /'
echo "scan: versions per table in a record-time window:"
for w in cold warm; do $C --time -q "SELECT tbl, count(), countIf(ret <> 0) FROM version WHERE rec BETWEEN 1767225600000000 AND 1767226100000000 GROUP BY tbl FORMAT Null" 2>&1 | sed "s/^/  $w /"; done
$C -q "SELECT formatReadableSize(sum(bytes_on_disk)) FROM system.parts WHERE table = 'version' AND active" | sed 's/^/  size on disk: /'
