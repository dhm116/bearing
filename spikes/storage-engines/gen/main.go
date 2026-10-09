// Command gen writes the benchmark rows as TSV, shaped like surrealstore's
// version table: tbl, key, n, rec (µs), ret (µs, 0 = live) and about 300
// bytes of data.
//
//	go run ./gen seed 200000 > seed.tsv  # 200,000 keys, 5 versions each
//	go run ./gen apply 50000 > apply.tsv # 50,000 new rows at one record time
package main

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
)

const base = 1_767_225_600_000_000

var tbls = []string{"binding", "support", "fact", "state"}

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "seed" && os.Args[1] != "apply") {
		fmt.Fprintln(os.Stderr, "usage: gen seed|apply COUNT")
		os.Exit(2)
	}
	count, err := strconv.Atoi(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(2)
	}
	r := rand.New(rand.NewPCG(1, 0))
	blob := make([]byte, 300)
	data := func() []byte {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
		for i := range blob {
			blob[i] = letters[r.IntN(len(letters))]
		}
		return blob
	}
	w := bufio.NewWriter(os.Stdout)
	if os.Args[1] == "seed" {
		// Each key gets five versions, each superseded by the next.
		for k := range count {
			for n := range 5 {
				rec := base + int64(k*5+n)*1000
				ret := rec + 1000
				if n == 4 {
					ret = 0
				}
				fmt.Fprintf(w, "%s\tacme/k%d\t%d\t%d\t%d\t%s\n", tbls[k%4], k, n, rec, ret, data())
			}
		}
	} else {
		rec := int64(base + 1_000_000_000_000)
		for k := range count {
			fmt.Fprintf(w, "%s\tnew/k%d\t0\t%d\t0\t%s\n", tbls[k%4], k, rec, data())
		}
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}
