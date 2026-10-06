#!/usr/bin/env bash
# Starts a throwaway regtest bitcoind + Fulcrum for end-to-end testing.
# Everything here is synthetic: a fresh chain mined locally.
set -euo pipefail
NET=btctax-regtest
docker network inspect $NET >/dev/null 2>&1 || docker network create $NET >/dev/null
docker rm -f btctax-bitcoind btctax-fulcrum >/dev/null 2>&1 || true
docker run -d --name btctax-bitcoind --network $NET ruimarinho/bitcoin-core:24 \
  -regtest=1 -server=1 -txindex=1 -fallbackfee=0.0002 \
  -rpcuser=test -rpcpassword=test -rpcbind=0.0.0.0 -rpcallowip=0.0.0.0/0 >/dev/null
cli() { docker exec btctax-bitcoind bitcoin-cli -regtest -rpcuser=test -rpcpassword=test "$@"; }
for i in $(seq 1 30); do cli getblockchaininfo >/dev/null 2>&1 && break; sleep 1; done
cli createwallet miner >/dev/null
cli -rpcwallet=miner generatetoaddress 101 "$(cli -rpcwallet=miner getnewaddress)" >/dev/null
docker run -d --name btctax-fulcrum --network $NET -p 127.0.0.1:50001:50001 -p 127.0.0.1:50002:50002 cculianu/fulcrum:latest \
  Fulcrum -b btctax-bitcoind:18443 -u test -p test -t 0.0.0.0:50001 -s 0.0.0.0:50002 >/dev/null
echo "regtest up: Fulcrum on 127.0.0.1:50001 (TCP) and :50002 (TLS, self-signed)"
