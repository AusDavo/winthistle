# Winthistle. `make check` is the one you want.
.RECIPEPREFIX := >
.DEFAULT_GOAL := help
.PHONY: help build test test-unit check harness fmt vet

help:  ## show this help
> @grep -hE '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | expand -t14

build:  ## build the binary
> go build ./...

fmt:  ## format
> gofmt -l -w .

vet:  ## vet
> go vet ./...

test-unit:  ## tests that need no harness
> go test -short -count=1 ./...

test:  ## everything; harness-backed tests skip if regtest is down
> go test -count=1 -timeout 20m ./...

check: vet test  ## vet and test

harness:  ## rebuild the regtest cluster from scratch (~1 min)
> $(MAKE) -C regtest reset
