# go-secs v1 vs v2 benchmarks

A standalone Go module (separate `go.mod`, not part of the public `go-secs`
module graph) that benchmarks the current v2 working tree against the latest
published v1 release, side by side.

- `github.com/arloliu/go-secs` (v1) is pulled at its latest tag.
- `github.com/arloliu/go-secs/v2` is `replace`d to `../` — the v2 source tree
  this module lives next to, so it always benchmarks whatever is currently
  checked out, not a published v2 release.

## Layout

```
secs2item/{v1,v2}/     secs2.Item construct / encode / decode, no I/O
hsmsssdata/{v1,v2}/    full active/passive HSMS-SS pair over real loopback
                       TCP, send+reply round trips
```

Each `v1`/`v2` pair defines **identically named** benchmarks over identical
payload shapes (small ack, 100k-leaf structured list, 100k-byte wafer map,
1 MiB recipe ASCII blob) so `benchstat` can diff the two result files
directly. Where the library APIs are identical between versions (most of the
`secs2` shortcut constructors) the benchmark bodies are copied verbatim,
differing only in import path; where they differ (item pooling/`Free()` in
v1 vs. immutable/unpooled in v2, `Session`-per-connection in v1 vs. a
connection-is-its-own-endpoint in v2) each side uses its own idiom — see the
comments in each `bench_test.go`.

## Running

```sh
make bench-v1       # secs2item + hsmsssdata, v1, -> results/v1.txt
make bench-v2       # secs2item + hsmsssdata, v2, -> results/v2.txt
make compare        # benchstat results/v1.txt results/v2.txt
```

Or run a single package directly, e.g.:

```sh
go test -run=^$ -bench=. -benchmem ./secs2item/v1/...
go test -run=^$ -bench=. -benchmem -benchtime=20x ./hsmsssdata/v2/...
```

`v1` and `v2` benchmarks live in different Go packages (necessarily — they
import different module major versions), so their `pkg:` lines differ.
Plain `benchstat v1.txt v2.txt` therefore treats them as unrelated and prints
separate tables instead of a side-by-side diff; pass `-ignore pkg` (as
`make compare` does) so benchstat aligns rows by benchmark name only.

`make bench-v1` / `make bench-v2` run each benchmark 6 times (`-count=6`,
benchstat's minimum for a confidence interval) by default; override with
`make bench-v1 COUNT=20` for a tighter interval.

`hsmsssdata` benchmarks spin up a real TCP handshake outside the timed loop
on every calibration call Go's testing harness makes, so prefer a fixed
`-benchtime=Nx` (e.g. `20x`) over the time-based default to keep total run
time predictable.

`results/` is git-ignored scratch output, not checked in.
