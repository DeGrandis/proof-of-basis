#!/usr/bin/env bash
# Plays a synthetic story on the regtest chain and writes matching exchange
# CSVs to $OUT: buy on "Coinbase", withdraw to cold storage, pay a coffee
# shop, deposit to "River", sell there. Prints the cold wallet's tpub.
set -euo pipefail
OUT=${1:-./out}
mkdir -p "$OUT"
cli() { docker exec btctax-bitcoind bitcoin-cli -regtest -rpcuser=test -rpcpassword=test "$@"; }
mine() { cli -rpcwallet=miner generatetoaddress "${1:-1}" "$(cli -rpcwallet=miner getnewaddress)" >/dev/null; }
blocktime() { cli getblock "$(cli gettransaction "$1" 2>/dev/null | jq -r .blockhash || cli -rpcwallet=miner gettransaction "$1" | jq -r .blockhash)" | jq -r .time; }
fmt() { date -u -d "@$1" '+%Y-%m-%d %H:%M:%S'; }

cli createwallet cold >/dev/null
TPUB=$(cli -rpcwallet=cold listdescriptors | jq -r '.descriptors[] | select(.desc|startswith("wpkh(")) | select(.internal==false) | .desc' | sed -E 's/.*\](tpub[1-9A-Za-z]+).*/\1/')

# 1. "Coinbase" withdraws 0.2 BTC to cold storage
COLD1=$(cli -rpcwallet=cold getnewaddress)
T1=$(cli -rpcwallet=miner sendtoaddress "$COLD1" 0.2)
mine
TIME1=$(cli -rpcwallet=miner gettransaction "$T1" | jq -r .blocktime)

# 2. cold pays a coffee shop 0.001 BTC
SHOP=$(cli -rpcwallet=miner getnewaddress)
T2=$(cli -rpcwallet=cold sendtoaddress "$SHOP" 0.001)
mine

# 3. cold deposits 0.1 BTC to "River"
RIVER=$(cli -rpcwallet=miner getnewaddress)
T3=$(cli -rpcwallet=cold sendtoaddress "$RIVER" 0.1)
mine
TIME3=$(cli -rpcwallet=miner gettransaction "$T3" | jq -r .blocktime)

BUY=$((TIME1 - 86400*400))  # bought 400 days earlier: long-term by sale time
cat > "$OUT/coinbase.csv" <<CSV
Transactions
User,synthetic,00000000-0000-0000-0000-000000000000
ID,Timestamp,Transaction Type,Asset,Quantity Transacted,Price Currency,Price at Transaction,Subtotal,Total (inclusive of fees and/or spread),Fees and/or Spread,Notes
syn1,$(fmt $BUY) UTC,Buy,BTC,0.50000000,USD,\$40000.00,\$20000.00,\$20100.00,\$100.00,Bought 0.5 BTC
syn2,$(fmt $((TIME1 - 120))) UTC,Send,BTC,-0.20000000,USD,\$60000.00,\$12000.00,\$12000.00,\$0.00,Sent 0.2 BTC to $COLD1
CSV
cat > "$OUT/river.csv" <<CSV
Date,Sent Amount,Sent Currency,Received Amount,Received Currency,Fee Amount,Fee Currency,Tag
$(fmt $((TIME3 + 600))),,,0.10000000,BTC,,,Deposit
$(fmt $((TIME3 + 3600))),0.10000000,BTC,9000.00,USD,45.00,USD,Sell
CSV
echo "$TPUB"
