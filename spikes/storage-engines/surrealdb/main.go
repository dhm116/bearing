// Command surrealdb runs the storage-engine benchmark against a SurrealDB
// server over HTTP, with the table, indexes and chunking of
// internal/surrealstore: 500 rows per INSERT and 2,000 per transaction.
//
//	surreal start --user root --pass root --bind 127.0.0.1:8000 surrealkv://DIR
//	go run . -data DATA_DIR
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var server = flag.String("server", "http://127.0.0.1:8000", "SurrealDB HTTP endpoint")

// q runs SurrealQL as root in namespace and database bench.
func q(sql string) (time.Duration, error) {
	req, err := http.NewRequest(http.MethodPost, *server+"/sql", strings.NewReader(sql))
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth("root", "root")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("surreal-ns", "bench")
	req.Header.Set("surreal-db", "bench")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	d := time.Since(start)
	if err != nil {
		return d, err
	}
	if resp.StatusCode != http.StatusOK || bytes.Contains(b, []byte(`"status":"ERR"`)) {
		return d, fmt.Errorf("%s", b[:min(len(b), 400)])
	}
	return d, nil
}

func must(d time.Duration, err error) time.Duration {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return d
}

type row struct {
	Tbl  string `json:"tbl"`
	Key  string `json:"key"`
	N    int    `json:"n"`
	Rec  int64  `json:"rec"`
	Ret  int64  `json:"ret"`
	Data string `json:"data"`
}

func read(path string) []row {
	f, err := os.Open(path)
	if err != nil {
		must(0, err)
	}
	defer f.Close()
	var out []row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		n, _ := strconv.Atoi(p[2])
		rec, _ := strconv.ParseInt(p[3], 10, 64)
		ret, _ := strconv.ParseInt(p[4], 10, 64)
		out = append(out, row{p[0], p[1], n, rec, ret, p[5]})
	}
	return out
}

// tx writes rows in one transaction, 500 per INSERT.
func tx(rows []row) time.Duration {
	var sb strings.Builder
	sb.WriteString("BEGIN TRANSACTION;\n")
	for i := 0; i < len(rows); i += 500 {
		b, _ := json.Marshal(rows[i:min(i+500, len(rows))])
		sb.WriteString("INSERT INTO version ")
		sb.Write(b)
		sb.WriteString(";\n")
	}
	sb.WriteString("COMMIT TRANSACTION;")
	return must(q(sb.String()))
}

// staged writes rows in transactions of 2,000, as a large surrealstore apply does.
func staged(rows []row) time.Duration {
	var total time.Duration
	for i := 0; i < len(rows); i += 2000 {
		total += tx(rows[i:min(i+2000, len(rows))])
	}
	return total
}

func main() {
	dir := flag.String("data", ".", "directory holding seed.tsv and apply.tsv from gen.py")
	seed := flag.Bool("seed", true, "recreate and seed the table; false reuses an earlier run's")
	flag.Parse()
	if *seed {
		must(q(`DEFINE NAMESPACE IF NOT EXISTS bench; USE NS bench; DEFINE DATABASE IF NOT EXISTS bench;`))
		must(q(`REMOVE TABLE IF EXISTS version; DEFINE TABLE version SCHEMALESS;
DEFINE INDEX version_key ON version FIELDS tbl, key; DEFINE INDEX version_rec ON version FIELDS rec;`))
		fmt.Printf("seed 1M rows in transactions of 2,000:\n  %.1fs\n", staged(read(*dir+"/seed.tsv")).Seconds())
		fmt.Printf("apply 50,000 new rows, staged in 25 transactions of 2,000:\n  %.1fs\n", staged(read(*dir+"/apply.tsv")).Seconds())
	}

	var keys []map[string]any
	for k := 2; len(keys) < 5000; k += 4 {
		keys = append(keys, map[string]any{"tbl": "fact", "key": fmt.Sprintf("acme/k%d", k)})
	}
	kb, _ := json.Marshal(keys)
	d := must(q("BEGIN TRANSACTION;\nFOR $r IN " + string(kb) + " { UPDATE version SET ret = 1800000000000000 WHERE tbl = $r.tbl AND key = $r.key AND n = 4; };\nCOMMIT TRANSACTION;"))
	fmt.Printf("supersede 5,000 live rows in one transaction, as commit does:\n  %.2fs\n", d.Seconds())

	fmt.Println("point read of one series as of a record time (10 s each):")
	for _, c := range []int{1, 16} {
		var mu sync.Mutex
		var lat []time.Duration
		end := time.Now().Add(10 * time.Second)
		var wg sync.WaitGroup
		for range c {
			wg.Go(func() {
				for time.Now().Before(end) {
					k := 4*rand.IntN(50000) + 2
					d := must(q(fmt.Sprintf(`SELECT n, rec, ret, data FROM version WHERE tbl = 'fact' AND key = 'acme/k%d' AND rec <= 1767226600000000 AND (ret = 0 OR ret > 1767226600000000);`, k)))
					mu.Lock()
					lat = append(lat, d)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		slices.Sort(lat)
		fmt.Printf("  c=%d %.0f/s p50 %.2fms p99 %.2fms\n", c, float64(len(lat))/10, float64(lat[len(lat)/2].Microseconds())/1000, float64(lat[len(lat)*99/100].Microseconds())/1000)
	}

	fmt.Println("load two series with key IN [...], surrealstore's load shape, and the same keys as OR'd equalities:")
	in := must(q(`SELECT tbl, key, n, rec, ret, data FROM version WHERE rec <= 1800000000000000 AND tbl = 'fact' AND key IN ['acme/k2', 'acme/k6'] ORDER BY n;`))
	or := must(q(`SELECT tbl, key, n, rec, ret, data FROM version WHERE (tbl = 'fact' AND key = 'acme/k2') OR (tbl = 'fact' AND key = 'acme/k6') ORDER BY n;`))
	fmt.Printf("  IN %.3fs, OR %.3fs\n", in.Seconds(), or.Seconds())

	fmt.Println("scan: versions per table in a record-time window:")
	for _, w := range []string{"cold", "warm"} {
		d := must(q(`SELECT tbl, count() AS c FROM version WHERE rec >= 1767225600000000 AND rec <= 1767226600000000000 GROUP BY tbl;`))
		fmt.Printf("  %s %.2fs\n", w, d.Seconds())
	}
}
