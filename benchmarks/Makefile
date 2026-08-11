.PHONY: bench-v1 bench-v2 bench-compression profile-v2 test-compat compare clean

COUNT ?= 6
# Must stay a multiple of 32 (BenchmarkConnectionPool_ConcurrentRoundTrip's
# profileNumConns * profileWorkersPerConn) so the benchmark's reported op
# count matches the actual number of round trips performed exactly.
PROFILE_ITERS ?= 2048

results:
	@mkdir -p results

bench-v1: results
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) ./secs2item/v1/... > results/secs2item_v1.txt
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) -benchtime=20x ./hsmsssdata/v1/... > results/hsmsssdata_v1.txt
	cat results/secs2item_v1.txt results/hsmsssdata_v1.txt > results/v1.txt

bench-v2: results
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) ./secs2item/v2/... > results/secs2item_v2.txt
	go test -run=^$$ -bench=BenchmarkConnection_ -benchmem -count=$(COUNT) -benchtime=20x ./hsmsssdata/v2/... > results/hsmsssdata_v2.txt
	cat results/secs2item_v2.txt results/hsmsssdata_v2.txt > results/v2.txt

# Codec characterization for the HSMS payload-compression study
# (docs/specs/2026-08-11-hsms-payload-compression-research.md, §8).
#
# Deliberately outside bench-v1/bench-v2/compare: these are absolute codec
# figures on a modelled SECS-II corpus, not a v1-v2 parity run, so benchstat
# has nothing to diff them against. count=1 for the same reason.
#
# The dictionary cases shell out to the reference `zstd` CLI to train a COVER
# dictionary; without it on PATH they degrade to the no-dictionary codecs
# rather than failing.
COMPRESSION_BENCHTIME ?= 0.2s

bench-compression: results
	go test -run='TestRatios|TestHoldoutDict|TestItemLevelVsWholeBody' -v -count=1 \
	  ./compression/... > results/compression_tables.txt
	go test -run=^$$ -bench=. -benchmem -count=1 -cpu=1 -benchtime=$(COMPRESSION_BENCHTIME) \
	  ./compression/... > results/compression_bench.txt

profile-v2: results
	go test -run=^$$ -bench=BenchmarkConnectionPool_ConcurrentRoundTrip -benchmem \
	  -benchtime=$(PROFILE_ITERS)x -cpuprofile=results/cpu_v2.prof -memprofile=results/mem_v2.prof \
	  -blockprofile=results/block_v2.prof -mutexprofile=results/mutex_v2.prof \
	  ./hsmsssdata/v2/... > results/profile_v2.txt

test-compat:
	go test ./compat -run Compatibility -v

compare:
	benchstat -ignore pkg results/v1.txt results/v2.txt

clean:
	rm -rf results
