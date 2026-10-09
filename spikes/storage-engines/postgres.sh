#!/bin/bash
# Runs the storage-engine benchmark on PostgreSQL. Usage:
#   postgres.sh DATA_DIR   (DATA_DIR holds seed.tsv and apply.tsv from gen.py)
# Connects with PG* environment variables, e.g. PGHOST=/tmp PGPORT=5433
# PGUSER=postgres; needs psql and pgbench on PATH.
set -e
D=$1
P="psql -q -v ON_ERROR_STOP=1"
tm() { local s; s=$(date +%s.%N); "$@"; printf "  %.2fs\n" "$(echo "$(date +%s.%N) - $s" | bc)"; }
ms() { $P -c '\timing on' -c "$1" | grep Time | sed "s/^/  $2/"; }

$P -c "DROP TABLE IF EXISTS version" 2>/dev/null
$P -c "CREATE TABLE version (tbl text, key text, n int, rec bigint, ret bigint, data text);
CREATE INDEX version_key ON version (tbl, key); CREATE INDEX version_rec ON version (rec);"
echo "seed 1M rows (200,000 series of 5 versions):"; tm $P -c "\copy version FROM '$D/seed.tsv'"
$P -c "VACUUM ANALYZE version"
printf "BEGIN;\n\\\\copy version FROM '%s/apply.tsv'\nCOMMIT;\n" "$D" > "$D/apply.psql"
echo "apply 50,000 new rows in one transaction:"; tm $P -f "$D/apply.psql"
echo "supersede 5,000 live rows (set ret) in one transaction:"
tm $P -c "UPDATE version v SET ret = 1800000000000000 FROM (SELECT tbl, key FROM version WHERE n = 4 AND ret = 0 LIMIT 5000) s WHERE v.tbl = s.tbl AND v.key = s.key AND v.n = 4"
cat > "$D/point.sql" <<'Q'
\set k random(0, 49999)
SELECT n, rec, ret, data FROM version WHERE tbl = 'fact' AND key = 'acme/k' || (4 * :k + 2) AND rec <= 1767226600000000 AND (ret = 0 OR ret > 1767226600000000);
Q
echo "point read of one series as of a record time (10 s each):"
for c in 1 16; do pgbench -n -c $c -j $c -T 10 -f "$D/point.sql" 2>/dev/null | grep -E "latency average|^tps" | sed "s/^/  c=$c /"; done
echo "load two series with key IN (...), surrealstore's load shape:"
ms "SELECT tbl, key, n, rec, ret, data FROM version WHERE rec <= 1800000000000000 AND tbl = 'fact' AND key IN ('acme/k2', 'acme/k6') ORDER BY n"
echo "scan: versions per table in a record-time window:"
for w in cold warm; do ms "SELECT tbl, count(*), count(*) FILTER (WHERE ret <> 0) FROM version WHERE rec BETWEEN 1767225600000000 AND 1767226600000000000 GROUP BY tbl" "$w "; done
$P -t -c "SELECT pg_size_pretty(pg_total_relation_size('version'))" | sed 's/^ */  size on disk: /'
