# BTC Tax

**Your bitcoin's tax story, worked out on your own server.**

BTC Tax follows your coins from the exchanges where you bought them, through
your own wallets, to where you sold or spent them. It works out your cost
basis, gains and income, flags anything it can't explain, and when tax
season comes it walks you through gathering everything and hands your tax
professional one tidy package.

It runs on your own machine and talks only to your own Bitcoin node. No
account, no cloud, no tracking.

> **Not tax advice.** BTC Tax is a self-help tool for organizing your
> records. It is not a CPA, enrolled agent, law firm or tax preparer, and its
> numbers may be incomplete or wrong. You are responsible for your own taxes;
> check everything with a qualified tax professional before you file. See the
> full [Disclaimer](#disclaimer).

<!-- Screenshot: Overview (holdings chart and the "Now" card) — docs/images/overview.png -->

---

## Why BTC Tax

- **Private by design.** It reads your wallets through your own Fulcrum
  server, keeps everything in one file on your machine, and never phones
  home. The package for your tax professional leaves out transaction IDs and
  addresses unless you choose to include them.
- **It follows the coins.** Buys on an exchange, withdrawals to cold storage,
  spends from a hot wallet, sales back on an exchange: it links them up, so
  every sale knows which purchase it came from.
- **Ready for tax season.** Five guided steps take you from "is everything
  here?" to a download for your tax professional, with your exchanges'
  1099-DA forms checked against your own records.
- **Works on your phone.** Labeling, the charts and record pages are built
  for a phone screen too.

---

## What you can do

### See where you stand
The **Overview** shows what you hold, what it cost, and how it has changed,
with every buy and sale on the chart. Tap one to see its details. Warnings
point at anything that needs attention, like an account that seems to go
below zero.

<!-- Screenshot: Overview warnings and accounts — docs/images/overview-accounts.png -->

### Label what it couldn't match
Some transactions can't be matched automatically, like a payment to a
merchant or a gift. **Review** lists them. On a phone it shows one at a
time with big buttons ("Paid for something", "Gift", "To my own wallet"…),
and moves to the next as you go.

<!-- Screenshot: Review on a phone (one item at a time) — docs/images/review-mobile.png -->

### Watch your coins move
**Flow** draws each account as a lane over time, with every buy, transfer,
sale and spend on it. Pinch or scroll to zoom. Tap anything to see where
those coins came from and where they went.

<!-- Screenshot: Flow with a record popup open — docs/images/flow.png -->

### Open any transaction
Every record has its own page: what happened in plain words, the value,
basis and gain (with the % change), each lot the coins came from and the
route they took, the on-chain transaction or the exchange's original CSV
row, and room to attach a receipt or letter as proof.

<!-- Screenshot: A record page — docs/images/record.png -->

### Get ready for tax season
**Taxes** opens on the current tax season and walks you through five steps:

1. **Complete your records.** Everything imported, nothing unlabeled, every
   sale traced to a purchase.
2. **Add your 1099-DA forms.** Drop each exchange's PDF and enter its total.
   It's checked against your records.
3. **Fill in income and gifts.** What kind of income each was, who gifts and
   donations went to, and the January 1, 2025 per-wallet split the IRS now
   expects.
4. **Check your Form 8949.** Every sale in the right box, ready to review.
5. **Download for your tax pro.** One zip with a README for them, the Form
   8949 lines, the 1099-DA PDFs and how each was checked, where every coin
   came from, income, gifts, transfers and your proof files.

<!-- Screenshot: The five tax season steps — docs/images/taxes.png -->

### Plan before you sell
**Plan a sale** shows which coins a sale would use, the gain on each, when
short-term coins turn long-term, and a rough tax estimate. You can record
which coins you chose before you sell, as the IRS asks.

### And more
- **Lightning**: connect a Core Lightning node (read-only) to include its
  payments, receipts and routing income.
- **Assistant** (optional): ask questions about your data, using an AI model
  on your own Ollama server. It only changes things when you press Apply.
- **Undo anything**: every change (yours or the assistant's) is listed in
  Activity and can be undone.
- **Backups**: one encrypted file with everything, and restore.
- **Light and dark** themes.

---

## Getting started

You need Docker, and a Fulcrum (Electrum) server for your wallets. Most
node packages (Umbrel, Start9, RaspiBlitz, myNode) offer one.

1. **Start it.** In this folder:
   ```sh
   docker compose up -d --build
   ```
   Then open `http://<this computer's address>:8080`.
2. **Set a password.** The first visit asks for one. Do this right away.
3. **Connect your node.** In **Settings**, enter your Fulcrum address and
   port.
4. **Add your wallets.** In **Wallets**, paste each wallet's public key
   (xpub, ypub or zpub) or its output descriptor, and scan.
5. **Import your exchanges.** In **Imports**, drop the CSV exports from your
   exchanges. Several at once is fine.
6. **Label the leftovers** in **Review**.
7. **When tax season comes**, follow the steps on **Taxes**.

<!-- Screenshot: Settings, connecting Fulcrum — docs/images/settings.png -->

### What it supports

| | |
|---|---|
| **Exchanges** | Coinbase, River, Strike, Fold (CSV exports, detected automatically) |
| **Wallets** | Single-key (xpub, ypub, zpub, taproot) and multisig, by public key or output descriptor (Sparrow, Specter, Nunchuk, Bitcoin Core, Electrum, Caravan) |
| **Lightning** | Core Lightning, through CLNRest |
| **Tax rules** | United States: FIFO, LIFO or specific lot; per-wallet basis from 2025; Form 8949 boxes including the new digital asset boxes; capital loss limit and carryover |

### Good to know

- **It doesn't file your taxes, and it isn't tax advice.** It organizes and
  documents. Your tax professional prepares the return. See the
  [Disclaimer](#disclaimer).
- **Forgot your password?** On the server, run
  `docker compose exec btctax /btctax reset-password`, then set a new one on
  your next visit. Your data is untouched.
- **Using it away from home?** Put it behind HTTPS (a reverse proxy, or
  Tailscale). Over plain `http://` your password isn't encrypted on the way.

---

## Disclaimer

BTC Tax is software for organizing your own records, for informational and
educational purposes only.

- **Not professional advice.** It does not provide tax, legal, accounting or
  financial advice. Using it does not create a professional-client
  relationship with anyone.
- **Not a CPA, enrolled agent, law firm or tax preparer.** It does not
  prepare or file tax returns and does not guarantee any tax outcome.
- **Its numbers are estimates.** Basis, gains, income, tax estimates, Form
  8949 lines, 1099-DA checks and the assistant's answers depend on the
  records you give it, outside price data and its own rules. They may be
  incomplete or wrong.
- **You are responsible.** You alone are responsible for checking your
  numbers, knowing current tax law, your filings, and any tax, interest or
  penalties owed. Tax law is complex and changes often.
- **Talk to a professional.** Review your records and tax decisions with a
  qualified CPA, enrolled agent or tax attorney before you file.
- **No warranty.** The software is provided as is, without warranty of any
  kind. Its authors are not liable for any loss arising from its use.

The app shows this on first use, and it is always one click away in the
footer.

---

## Technical details

### Configuration

| Variable | Default | |
|---|---|---|
| `BTCTAX_ADDR` | `127.0.0.1:8080` (the image uses `0.0.0.0:8080`) | Listen address |
| `BTCTAX_DATA` | `./data` (the image uses `/data`) | Data directory; everything is one SQLite file here |
| `BTCTAX_PASSWORD` | unset | If set, becomes the login password each time the app starts |

The app is one static Go binary with the UI embedded; the Docker image is
built `FROM scratch`. `docker-compose.yml` publishes port 8080 on your LAN
and keeps data in the `btctax-data` volume.

### Login and sessions

The password is stored only as a salted PBKDF2-SHA256 hash. Sessions are
random tokens in an HttpOnly cookie that stay valid for 30 days after last
use, and the cookie is marked secure when served over HTTPS. Repeated wrong
passwords lock out that address for a while. Changing the password signs
out every other device; Settings also has **Sign out everywhere**.
`reset-password` clears the password and all sessions.

### Network and privacy

At runtime the app contacts only:

- your Fulcrum server (wallet scans; its TLS certificate is pinned on first
  use),
- your Core Lightning node, when you sync (optional),
- your Ollama server, when you use the assistant (optional),
- a price source, only when you press **Update prices**.

Exports and the accountant package refer to records by HMAC references
(`R-XXXXXXXXXX`) instead of transaction IDs and addresses, unless you tick
**Include on-chain transaction IDs**. The app can look a reference up.

### Prices

Spends and income need a fair market value. Daily BTC/USD closes come from,
in order: prices you imported or updated, the history built into the binary,
then prices implied by your own trades that day.

The image build fetches the latest history (`--build-arg PRICE_SOURCE=yahoo`
by default, `coinmetrics` for data back to 2010, `none` to skip) and falls
back to the committed `internal/prices/data/btcusd-daily.csv` if the source
is unreachable. To refresh the committed file:

```sh
go run ./cmd/btctax prices fetch -source yahoo
go run ./cmd/btctax prices fetch -source coinmetrics -from 2010-07-17   # fills 2010-2014
```

New sources implement `prices.Source`.

### Matching and basis

- **Tracing**: exchange to wallet, wallet to exchange, wallet to wallet and
  exchange to exchange; by txid, then address (where the amount fits), then
  a unique amount and time, then an amount short by a network fee the export
  folds in or reports only in USD. Exchange sends that confirm up to two
  weeks late still match. A wrong automatic match can be unlinked.
- **Basis**: one pool before 2025, per wallet and account from January 1,
  2025 (Rev. Proc. 2024-28), with the split recorded and confirmable.
  Network fees on your own transfers count as a small sale (IRS FAQs 81 and
  97) or are added to basis, per Settings. Tax years and holding periods
  follow your time zone.
- **1099-DA**: PDFs are stored with their totals; a CSV of 1099-DA lines,
  where an exchange offers one, is matched sale by sale and drives the Form
  8949 boxes and column (g) adjustments.

### Assistant

Settings > Assistant connects a model on your own
[Ollama](https://ollama.com) server. It uses read-only tools to look things
up and proposes changes that happen only when you press Apply. Chats are
saved in the database, included in backups and removed by Wipe; prompts are
never logged.

- Pick a model that can use tools, such as `qwen3:8b`, `llama3.1:8b` or
  `gemma4:e4b`. Settings greys out models that can't.
- If Ollama runs on the same machine as the container, use
  `http://host.docker.internal:11434`. Ollama on another machine must listen
  on the network (`OLLAMA_HOST=0.0.0.0`).
- "Memory for each question" is Ollama's context window. 16k tokens is a
  good start.

### Lightning

Connect Core Lightning in Settings with its CLNRest address and a rune that
can only read the bookkeeper:

```sh
lightning-cli createrune restrictions='[["method=getinfo","method=bkpr-listincome","method=bkpr-listbalances"]]'
```

The rune is stored in the database, never sent back to the browser and
never logged. Payments count as spends at market value and received
payments as income unless you label them otherwise; routing fees are
income; on-chain deposits and withdrawals match your wallets and exchanges.

### Development

```sh
go test ./...                       # unit tests, synthetic data only
go run ./cmd/btctax -addr 127.0.0.1:8080
```

A demo with a large invented history (seven years, three exchanges, four
wallets) for trying the UI. It only writes to an empty database:

```sh
docker volume create btctax-demo
docker run --rm -v btctax-demo:/data btctax:latest demo -data /data
docker run --rm -p 127.0.0.1:8081:8080 -v btctax-demo:/data btctax:latest
```

End to end on a throwaway regtest chain (bitcoind and Fulcrum in Docker):

```sh
test/regtest/up.sh
test/regtest/scenario.sh ./out      # plays a synthetic buy/withdraw/spend/deposit/sell story, prints a tpub
# Settings: Fulcrum 127.0.0.1:50002 TLS (or 50001 plain), Network: Regtest
# Add the printed tpub as P2WPKH, scan, import ./out/*.csv
```

`test/regtest/lightning.sh` adds two throwaway regtest Lightning nodes.

**Never commit real exports.** Test fixtures are invented; `*.real.csv` and
`private/` are git-ignored for local testing.

### Layout

```
cmd/btctax          entrypoint
internal/wallet     extended keys, descriptors, address derivation, Electrum script hashes
internal/electrum   minimal Electrum protocol client
internal/scan       gap-limit wallet scan, per-transaction summaries
internal/importers  CSV handlers per platform, 1099-DA lines
internal/trace      matching exchange records to chain activity, the Review queue
internal/lots       cost-basis engine
internal/tax        jurisdiction rules (US)
internal/prices     price sources (Yahoo, Coin Metrics) and the built-in history
internal/store      SQLite persistence
internal/app        HTTP API, report, tax forms, accountant package, assistant
web/static          UI (plain JavaScript, no build step)
```
