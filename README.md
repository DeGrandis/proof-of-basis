# btctax

A self-hosted Bitcoin tax calculator. It reads your exchange CSV exports and
your wallets' extended public keys, traces coins from exchange to
self-custody and back, and estimates realized gains, losses and tax.

- Talks only to **your own Fulcrum (Electrum) server**. No price APIs, no
  telemetry, no third-party assets, no AI.
- One static Go binary with the UI embedded; the Docker image is about 17 MB.
- All data lives in one SQLite file in `/data`.

## Run

```sh
docker compose up -d --build
# open http://127.0.0.1:8080
```

Then: **Settings** (Fulcrum host/port, TLS), **Wallets** (add each
xpub/ypub/zpub and scan), **Imports** (exchange CSVs), **Review** (label
anything that could not be matched), **Taxes**.

Environment: `BTCTAX_ADDR` (default `127.0.0.1:8080`, the image uses
`0.0.0.0:8080`), `BTCTAX_DATA` (default `./data`), `BTCTAX_PASSWORD`
(optional HTTP basic auth).

## What it does

| Area | Status |
|---|---|
| Wallets | xpub/ypub/zpub (and testnet forms) or an output descriptor: `pkh`, `wpkh`, `sh(wpkh)`, `tr(KEY)`, `wsh`/`sh(wsh)`/`sh` with `multi`/`sortedmulti`, `<0;1>` paths, checksum verified. Gap-limit scan via Fulcrum, TLS cert pinned on first use |
| Imports | Coinbase, River, Strike, Fold, auto-detected. Strike and Fold column names are best guesses until checked against real export headers |
| Tracing | Exchange withdrawal to wallet, wallet to exchange deposit, wallet to wallet, exchange to exchange; by txid, then address, then unique amount+time |
| Labeling | Spend, sold, gift, donation, lost, income, gift received, P2P purchase, refund, untracked wallet, manual match |
| Basis | FIFO, LIFO, specific lot; universal pool before 2025, per wallet from 1 Jan 2025 (Rev. Proc. 2024-28); network fee on own transfers carried into basis or treated as a disposal |
| Output | Per-year short/long gains, income, estimated tax at your rates, Form 8949-style CSV, flow-of-funds graph |

## Prices

Spends and income need a fair market value. Daily BTC/USD closes come from,
in order of preference: prices you imported or updated, the history baked
into the binary, then prices implied by your own trades that day.

The image build fetches the latest history (`--build-arg PRICE_SOURCE=yahoo`
by default, `coinmetrics` for data back to 2010, `none` to skip) and falls
back to the committed `internal/prices/data/btcusd-daily.csv` if the source
is unreachable. To refresh the committed file:

```sh
go run ./cmd/btctax prices fetch -source yahoo
go run ./cmd/btctax prices fetch -source coinmetrics -from 2010-07-17   # fills 2010-2014
```

At runtime the app contacts a price source only when you press **Update
prices** in Settings. New sources implement `prices.Source`.

## Develop

```sh
go test ./...                       # unit tests, synthetic data only
go run ./cmd/btctax -addr 127.0.0.1:8080
```

End-to-end on a throwaway regtest chain (bitcoind + Fulcrum in Docker):

```sh
test/regtest/up.sh
test/regtest/scenario.sh ./out      # plays a synthetic buy/withdraw/spend/deposit/sell story, prints a tpub
# Settings: Fulcrum 127.0.0.1:50002 TLS (or 50001 plain), Network: Regtest
# Add the printed tpub as P2WPKH, scan, import ./out/*.csv
```

**Never commit real exports.** Test fixtures are invented; `*.real.csv` and
`private/` are git-ignored for local manual testing.

## Layout

```
cmd/btctax          entrypoint
internal/wallet     extended key parsing, address derivation, Electrum script hashes
internal/electrum   minimal Electrum protocol client
internal/scan       gap-limit wallet scan, per-transaction summaries
internal/importers  CSV handler interface and per-platform handlers
internal/trace      matching exchange records to chain activity, labeling queue
internal/lots       cost-basis engine
internal/tax        jurisdiction rules (US)
internal/prices     price sources (Yahoo, Coin Metrics) and the baked-in history
internal/store      SQLite persistence
internal/app        HTTP API and report
web/static          UI (plain JS, no build step)
```
