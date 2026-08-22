# Winthistle regtest harness.  `make reset` is the one you want.
# Uses .RECIPEPREFIX so recipes are '>' rather than tabs.
.RECIPEPREFIX := >
.DEFAULT_GOAL := help
.PHONY: help up down clean reset bootstrap cold creds mine info logs ps

NODES := alice bob carol dave
N ?= 6

help:  ## show this help
> @grep -hE '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | expand -t22

up:  ## start the cluster and wait for healthchecks
> docker compose up -d --wait

down:  ## stop the cluster, keep chain and channel state
> docker compose down

clean:  ## stop and destroy all state, including volumes
> docker compose down -v
> rm -rf creds __pycache__

reset: clean up bootstrap cold creds  ## destroy everything and rebuild from scratch
> @echo
> @echo "Harness ready. alice is your node: localhost:10009"

bootstrap:  ## mature the chain, fund the nodes, connect alice to peers
> ./bootstrap.sh

cold:  ## build the simulated 2-of-2 multisig cold wallet
> ./cold-wallet.py

creds:  ## copy each node's tls.cert and admin.macaroon into ./creds
> @for n in $(NODES); do \
>   mkdir -p creds/$$n; \
>   docker cp wt-$$n:/home/lnd/.lnd/tls.cert creds/$$n/tls.cert; \
>   docker cp wt-$$n:/home/lnd/.lnd/data/chain/bitcoin/regtest/admin.macaroon \
>     creds/$$n/admin.macaroon; \
>   echo "creds/$$n/"; \
> done

mine:  ## mine blocks: make mine N=6
> @addr=$$(./bin/bcli -rpcwallet=miner getnewaddress); \
> ./bin/bcli generatetoaddress $(N) $$addr >/dev/null; \
> echo "height: $$(./bin/bcli getblockcount)"

info:  ## one-line status per node
> @echo "height: $$(./bin/bcli getblockcount)"
> @for n in $(NODES); do \
>   printf '%-6s ' $$n; \
>   ./bin/lncli $$n getinfo | python3 -c \
>     'import sys,json;d=json.load(sys.stdin);print(f"{d[\"identity_pubkey\"][:16]}… peers={d[\"num_peers\"]} active={d[\"num_active_channels\"]} pending={d[\"num_pending_channels\"]}")'; \
> done

logs:  ## follow logs (make logs S=alice for one service)
> docker compose logs -f --tail=80 $(S)

ps:  ## container status
> docker compose ps
