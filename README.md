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
(optional; see below).

### Login

The app always asks for a password. On the first visit it asks you to set
one; after that you log in, and the browser stays logged in for 30 days
after you last used it. Settings has **Change password**, **Sign out** and
**Sign out everywhere**. The password is stored only as a salted
PBKDF2-SHA256 hash, sessions are random tokens in an HttpOnly cookie, and
repeated wrong passwords lock out that address for a while.

Set the password yourself before anyone else on your network can by
opening the app right after starting it, or by setting `BTCTAX_PASSWORD`:
when set, it becomes the password each time the app starts.

Forgot it? On the server:

```
docker compose exec btctax /btctax reset-password
```

That clears the password and signs everyone out; the next visit sets a new
one. Your data is untouched. (Or set `BTCTAX_PASSWORD` and restart.)

Over plain `http://` the password crosses your network unencrypted when
you log in. To use the app away from home, put it behind HTTPS, for
example a reverse proxy or Tailscale; the session cookie is marked secure
automatically when it is served over HTTPS.

## What it does

| Area | Status |
|---|---|
| Wallets | xpub/ypub/zpub (and testnet forms) or an output descriptor: `pkh`, `wpkh`, `sh(wpkh)`, `tr(KEY)`, `wsh`/`sh(wsh)`/`sh` with `multi`/`sortedmulti`, `<0;1>` paths, checksum verified. Gap-limit scan via Fulcrum, TLS cert pinned on first use |
| Imports | Coinbase (incl. Card Spend as a disposal and converts into BTC), River, Strike (reversed transactions netted out), Fold (metadata and disclaimer rows skipped), auto-detected. Each row gets a stable ID, so importing an overlapping or longer export never duplicates rows |
| Tracing | Exchange withdrawal to wallet, wallet to exchange deposit, wallet to wallet, exchange to exchange; by txid, then address, then unique amount+time |
| Labeling | Spend, sold, gift, donation, lost, income, gift received, P2P purchase, refund, untracked wallet, manual match |
| Basis | FIFO, LIFO, specific lot; universal pool before 2025, per wallet from 1 Jan 2025 (Rev. Proc. 2024-28); network fee on own transfers carried into basis or treated as a disposal |
| Output | Per-year short/long gains, income, estimated tax at your rates, Form 8949-style CSV, flow-of-funds graph |

## Lightning (optional)

Connect a Core Lightning node in **Settings** with its CLNRest address and
a rune that can only read the bookkeeper:

```sh
lightning-cli createrune restrictions='[["method=getinfo","method=bkpr-listincome","method=bkpr-listbalances"]]'
```

The rune is stored in the database, never sent back to the browser and
never logged. The node is read only when you sync (or Recheck everything).
Payments count as spends at market value and received payments as income,
unless you label them otherwise; routing fees earned are income; on-chain
deposits and withdrawals match your wallets and exchanges as self transfers.
`test/regtest/lightning.sh` adds two throwaway regtest nodes for testing.

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

A large invented history (seven years, three exchanges, four wallets,
consolidations, batch payments, multi-wallet spends) for trying the UI. It
only writes to a database with no accounts:

```sh
docker volume create btctax-demo
docker run --rm -v btctax-demo:/data btctax:latest demo -data /data
docker run --rm -p 127.0.0.1:8081:8080 -v btctax-demo:/data btctax:latest
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
