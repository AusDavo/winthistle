# Winthistle. `make check` is the one you want.
.RECIPEPREFIX := >
.DEFAULT_GOAL := help
.PHONY: help build test test-unit check harness fmt vet lint

help:  ## show this help
> @grep -hE '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | expand -t14

build:  ## build the binary
> go build ./...

fmt:  ## format
> gofmt -l -w .

vet:  ## vet
> go vet ./...

# An executable file with no shebang is handed to /bin/sh, which does not refuse
# it — it interprets it. regtest/verify.py hit exactly that: /bin/sh read the
# backticks in its Python docstring as command substitution and fork-bombed the
# machine. The recipes now name their interpreter, so nothing here depends on a
# shebang; this target stops the next such file being committed at all.
lint:  ## refuse a tracked executable that has no shebang
> @fail=0; \
> for f in $$(git ls-files -s | awk '$$1=="100755" {sub(/^[^\t]*\t/,""); print}'); do \
>   [ -f "$$f" ] || continue; \
>   if [ "$$(head -c2 -- "$$f")" != '#!' ]; then \
>     echo "  no shebang: $$f"; fail=1; \
>   fi; \
> done; \
> if [ $$fail -ne 0 ]; then \
>   echo; \
>   echo "Executable, but /bin/sh will be the one to read it. Add a shebang."; \
>   exit 1; \
> fi; \
> echo "lint: every tracked executable has a shebang"

test-unit:  ## tests that need no harness
> go test -short -count=1 ./...

# -p 1 is not a performance knob. Two harness-backed packages now exist, and
# `go test ./...` runs package binaries concurrently by default — which puts two
# test processes through the *same* alice, where one test's plain channel open
# leases the coins another test's psbt_verify is about to be judged against. That
# fails with lnd's reserved-value error, which says nothing about the real cause.
test:  ## everything; harness-backed tests skip if regtest is down
> go test -p 1 -count=1 -timeout 20m ./...

check: lint vet test  ## lint, vet and test

harness:  ## rebuild the regtest cluster from scratch (~1 min)
> $(MAKE) -C regtest reset
