# btctax

A self-hosted Bitcoin cost-basis tracker. It reads your exchange CSV exports
and your wallets' extended public keys, traces coins from exchange to
self-custody and back, and works out realized gains, losses and income.

It untangles a messy history so that when you sell or spend, the
documentation is one download away: a package for your tax professional with
Form 8949 lines, a check against your exchanges' Forms 1099-DA, where every
coin came from, and the proof behind each number. It shows the impact of a
sale before you make it and records which coins you chose. It does not
prepare or file returns; your tax professional does that.

- Talks only to **your own Fulcrum (Electrum) server**. No price APIs, no
  telemetry, no third-party assets. AI is optional and off by default: it
  only ever uses a model on your own Ollama server.
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

### Assistant (optional)

Settings > Assistant connects a model running on your own
[Ollama](https://ollama.com) server. A chat button then appears on every
page. It knows which page you're on and can look things up (transactions,
tax years, accounts, Review items, prices, a what-if sale) with read-only
tools. It can also propose changes (labels, transfer links, protecting a
purchase, correcting a value), which happen only when you press Apply, and
can be undone from Activity. Your data goes only to the Ollama address
you enter. Chats are saved in the app's database, included in backups and
removed by Wipe; prompts are never written to the log.

- Pick a model that can use tools, such as `qwen3:8b`, `llama3.1:8b` or
  `gemma4:e4b`. Settings greys out models that can't.
- If Ollama runs on the same machine as the container, use
  `http://host.docker.internal:11434` (inside Docker, `localhost` is the
  container). Ollama on another machine must listen on the network
  (`OLLAMA_HOST=0.0.0.0`).
- "Memory for each question" is Ollama's context window. 16k tokens is a
  good start; more needs more RAM or VRAM on the Ollama machine.

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
| Tracing | Exchange withdrawal to wallet, wallet to exchange deposit, wallet to wallet, exchange to exchange; by txid, then address (where the amount fits), then a unique amount and time, then an amount short by a network fee the export folds in or gives only in USD; exchange sends that confirmed up to two weeks late. A wrong automatic match can be unlinked |
| Labeling | Spend, sold, gift, donation, lost, income, gift received, P2P purchase, refund, untracked wallet, manual match |
| Basis | FIFO, LIFO, specific lot; universal pool before 2025, per wallet from 1 Jan 2025 (Rev. Proc. 2024-28), with the split listed, assignable per purchase and confirmed as a dated record; network fee on own transfers treated as a disposal (IRS FAQs 81, 97) or carried into basis; tax years and holding periods in your time zone |
| Planning | Plan a sale or spend: the coins it would use, gain by term, days until short-term lots turn long-term, and the tax impact; record which coins you chose before the sale (IRS FAQ 82), applied when the sale arrives |
| Documents | Per tax year: Form 8949 lines in the right box (C/F; G-L for digital assets from 2025) with codes and column (g) adjustments from your exchanges' imported 1099-DAs; income by type; the Form 1040 digital asset answer with its reasons; gifts, donations and losses with what to check; a readiness checklist; and an accountant package (zip) with all of it, lot provenance, transfers, the source files' fingerprints and the proof files you attached to records |
| Output | Per-year short/long gains, income, capital loss limit and carryover, a rough tax impact at your rates, flow-of-funds graph |

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
