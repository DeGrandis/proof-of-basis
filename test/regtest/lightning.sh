#!/usr/bin/env bash
# Adds two throwaway Core Lightning nodes to the regtest chain from up.sh
# and plays a synthetic story: a bitcoind wallet "cold" funds node A
# (self transfer), A opens a channel to B, pays B (a spend), is paid by B
# (income) and withdraws to "cold" (self transfer). Prints a read-only rune
# for node A, whose CLNRest is at https://cln-a:3010 on the btctax-regtest
# network. Needs python3 for JSON. Everything here is synthetic.
set -euo pipefail
NET=btctax-regtest
cli() { docker exec btctax-bitcoind bitcoin-cli -regtest -rpcuser=test -rpcpassword=test "$@"; }
la() { docker exec cln-a lightning-cli --network=regtest "$@"; }
lb() { docker exec cln-b lightning-cli --network=regtest "$@"; }
mine() { cli -rpcwallet=miner generatetoaddress "${1:-1}" "$(cli -rpcwallet=miner getnewaddress)" >/dev/null; }
j() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }

for n in a b; do
  docker rm -f cln-$n >/dev/null 2>&1 || true
  docker run -d --name cln-$n --network $NET elementsproject/lightningd:latest --network=regtest \
    --bitcoin-rpcconnect=btctax-bitcoind --bitcoin-rpcuser=test --bitcoin-rpcpassword=test --bitcoin-rpcport=18443 \
    --alias=test-$n --bind-addr=0.0.0.0:9735 --clnrest-port=3010 --clnrest-host=0.0.0.0 --clnrest-protocol=https >/dev/null
done
for i in $(seq 1 30); do la getinfo >/dev/null 2>&1 && lb getinfo >/dev/null 2>&1 && break; sleep 1; done

cli createwallet cold >/dev/null 2>&1 || true
cli -rpcwallet=miner sendtoaddress "$(cli -rpcwallet=cold getnewaddress)" 0.5 >/dev/null
mine 1
cli -rpcwallet=cold sendtoaddress "$(la newaddr | j 'd["bech32"]')" 0.1 >/dev/null
cli -rpcwallet=miner sendtoaddress "$(lb newaddr | j 'd["bech32"]')" 0.1 >/dev/null
mine 6
BID=$(lb getinfo | j 'd["id"]')
la connect "$BID@cln-b:9735" >/dev/null
for i in $(seq 1 20); do la fundchannel "$BID" 3000000 >/dev/null 2>&1 && break; sleep 2; done
for i in $(seq 1 20); do mine 2; sleep 3; [ "$(la listpeerchannels | j 'd["channels"][0]["state"]')" = CHANNELD_NORMAL ] && break; done
sleep 3
la pay "$(lb invoice 250000000 coffee "Coffee at Cafe" | j 'd["bolt11"]')" >/dev/null
lb pay "$(la invoice 40000000 lunch "Friend paid back lunch" | j 'd["bolt11"]')" >/dev/null
la withdraw "$(cli -rpcwallet=cold getnewaddress)" 2000000 >/dev/null
mine 6
sleep 3
echo "cold tpub: $(cli -rpcwallet=cold listdescriptors | python3 -c "import json,sys,re; d=json.load(sys.stdin); print([re.search(r'\](tpub\w+)',x['desc']).group(1) for x in d['descriptors'] if x['desc'].startswith('wpkh(') and not x['internal']][0])")"
echo "rune: $(la createrune restrictions='[["method=getinfo","method=bkpr-listincome","method=bkpr-listbalances"]]' | j 'd["rune"]')"
