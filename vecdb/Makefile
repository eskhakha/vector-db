.PHONY: all build test race bench contention demo crash replicate rebalance splitbrain fmt vet lint bce clean

all: fmt vet test

build:
	go build ./...

# -count=1 defeats the test cache so flakes surface instead of hiding.
test:
	go test -race -count=1 ./...

# Benchmarks are code, not scripts. Pin the count so results are comparable
# across runs, and record the machine spec alongside any number you quote.
bench:
	go test -run=XXX -bench=. -benchmem -benchtime=200x -skip=Contention ./internal/...

demo:
	go run ./cmd/vecdemo

crash:
	go run ./cmd/vecdemo -crash

replicate:
	go run ./cmd/vecdemo -replicate

rebalance:
	go run ./cmd/vecdemo -rebalance

splitbrain:
	go run ./cmd/vecdemo -splitbrain

fmt:
	gofmt -l -w .

vet:
	go vet ./...

# Bounds-check elimination in the distance kernel is a claim, so make it
# checkable rather than a comment.
# Contention is excluded from `make bench`: it runs background writer goroutines
# for the whole measurement and takes far longer than the rest combined.
contention:
	go test -run=XXX -bench=Contention -benchtime=100x ./internal/store/

bce:
	go build -gcflags="-d=ssa/check_bce/debug=1" ./internal/index 2>&1 | head -20

clean:
	rm -rf ./data
	go clean -testcache
