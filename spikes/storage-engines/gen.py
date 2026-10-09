# Generates the benchmark rows, shaped like surrealstore's version table:
# tbl, key, n, rec (µs), ret (µs, 0 = live), data (~300 bytes).
#   python3 gen.py seed 200000 > seed.tsv; python3 gen.py apply 50000 > apply.tsv
import random, string, sys
random.seed(1)
mode, count = sys.argv[1], int(sys.argv[2])
tbls = ["binding", "support", "fact", "state"]
blob = lambda: "".join(random.choices(string.ascii_letters, k=300))
base = 1_767_225_600_000_000
out = sys.stdout
if mode == "seed":   # count keys x 5 versions, each superseded by the next
    for k in range(count):
        t = tbls[k % 4]
        for n in range(5):
            rec = base + (k * 5 + n) * 1000
            ret = 0 if n == 4 else rec + 1000
            out.write(f"{t}\tacme/k{k}\t{n}\t{rec}\t{ret}\t{blob()}\n")
else:                # one apply: count new rows at one record time
    rec = base + 10**12
    for k in range(count):
        out.write(f"{tbls[k % 4]}\tnew/k{k}\t0\t{rec}\t0\t{blob()}\n")
