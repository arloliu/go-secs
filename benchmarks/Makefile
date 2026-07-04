.PHONY: bench-v1 bench-v2 compare clean

COUNT ?= 6

results:
	@mkdir -p results

bench-v1: results
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) ./secs2item/v1/... > results/secs2item_v1.txt
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) -benchtime=20x ./hsmsssdata/v1/... > results/hsmsssdata_v1.txt
	cat results/secs2item_v1.txt results/hsmsssdata_v1.txt > results/v1.txt

bench-v2: results
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) ./secs2item/v2/... > results/secs2item_v2.txt
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) -benchtime=20x ./hsmsssdata/v2/... > results/hsmsssdata_v2.txt
	cat results/secs2item_v2.txt results/hsmsssdata_v2.txt > results/v2.txt

compare:
	benchstat -ignore pkg results/v1.txt results/v2.txt

clean:
	rm -rf results
