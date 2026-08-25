# Winthistle signet harness. `make up` starts it; `make sync` tells you how far
# along the initial block download is.
#
# Two bitcoinds and nothing else — see docker-compose.yml for why there is no
# LND here. The tests that use this skip unless WINTHISTLE_SIGNET=1 is set, so
# `make test` at the repo root stays green on a machine that has never run this.
.RECIPEPREFIX := >
.DEFAULT_GOAL := help
.PHONY: help up down clean sync cold address height info sweep logs ps

help:  ## show this help
> @grep -hE '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | expand -t22

up:  ## start both nodes (does NOT wait for the block download)
> docker compose up -d
> @echo
> @echo "Both nodes are up and downloading. This is the one unavoidable cost of"
> @echo "signet: watch it with 'make sync'. Nothing else here needs it finished."

down:  ## stop both nodes, keep the downloaded chain
> docker compose down

clean:  ## stop and destroy everything, including the downloaded chain
> docker compose down -v
> rm -rf __pycache__

sync:  ## how far through the block download each node is
> @python3 ./status.py

cold:  ## build the simulated 2-of-2 multisig cold wallet on the unpruned node
> python3 ./cold-wallet.py

address:  ## print a receive address for the cold wallet, to paste into a faucet
> @./bin/bcli -rpcwallet=cold-watch getnewaddress

height:  ## the block height of each coin the cold wallet holds — the tests' reference
> @python3 ./status.py --coins-only

info:  ## sync state and what the cold wallet holds
> @python3 ./status.py --coins

sweep:  ## delete the throwaway wallets the Go tests create
> @./bin/bcli listwalletdir | python3 -c \
>   'import sys,json;[print(w["name"]) for w in json.load(sys.stdin)["wallets"] if w["name"].startswith("winthistle-")]' \
>   | while read -r w; do \
>       ./bin/bcli unloadwallet "$$w" >/dev/null 2>&1 || true; \
>       docker compose exec -T bitcoind rm -rf "/home/bitcoin/.bitcoin/signet/wallets/$$w"; \
>       echo "removed $$w"; \
>     done

logs:  ## follow logs (make logs S=pruned for one node)
> docker compose logs -f --tail=80 $(S)

ps:  ## container status
> docker compose ps
