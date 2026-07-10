.PHONY: bench-v1 bench-v2 profile-v2 test-compat compare clean

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
