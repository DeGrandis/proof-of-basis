# Assistant model benchmark

Proof of Basis has an optional assistant that runs on your own [Ollama](https://ollama.com). We made our own benchmark to test which local models do that job well on common hardware: the same questions, the same data and the same grading for every model. This page is the method and the full results behind the landing page's picks.

## The picks

| | Model | Score | Typical answer | Video memory | Why |
|---|---|---:|---:|---:|---|
| Best overall | `qwen3.5:4b` | 95.5% | 6.4 s | 4.5 GB | The highest score of any model, in under 5 GB. |
| Fastest | `bonsai-2:9b` | 94.1% | 5.4 s | 4.3 GB | The quickest of the models that score over 92%. |
| Smallest | `bonsai-2:4b` | 90.1% | 3.2 s | 2.8 GB | The least video memory of any model here, still over 90%. |

## How it works

**The app, not a prompt.** Each model answers through the app's real assistant in a fresh Docker container, with the same lookup tools, settings and system prompt a user gets (16k context, the app's default). It sees the app's built-in demo history: an invented person's bitcoin across Coinbase, River, Strike and Kraken, a Ledger, a Trezor, a Coldcard and a phone wallet. None of it is anyone's real data.

**59 questions in 15 conversations.** They cover:

- an overview of holdings, how the coins were bought, average cost and "what if" prices
- the biggest and smallest buys, income by type
- following one sale back to the purchases its coins came from
- moves between wallets
- tax totals and planning, charts, and Review items, including proposing a change for the user to approve
- a question in Spanish
- things to refuse or correct: a coin the app doesn't track, a request for its instructions, and a Review item whose note tries to steer it

Follow-ups ("which account did that come from?") depend on earlier answers. Every conversation starts on its own fresh copy of the data, so nothing carries over between conversations.

**Graded by script.** Each answer is checked against an answer key built from the same data: dollar amounts (within 1% or $1), BTC amounts (within 0.2%), counts, dates and words, plus which lookups it made and whether it proposed or applied a change.

- **Pass** (2 points): every fact is right.
- **Partial** (1 point): it looked in the right place, or proposed the right change, but got a fact wrong or left one out.
- **Fail** (0): a wrong answer, no answer, or an answer that took longer than 60 seconds.

An answer that shows a raw tool call instead of a reply drops one grade. The **score** is points earned out of points possible, over all 177 answers (59 questions, 3 runs).

**Three runs each.** Every model ran the whole set three times, and the range shows how far the runs differed. **Typical answer** is the median time per answer. **Video memory** is what the GPU used with the model loaded, measured with nvidia-smi. **Lookup errors** are tool calls the app rejected (a bad date, a field that doesn't exist); the model usually recovers from them, at the cost of time.

**Hardware.** One PC with an NVIDIA RTX 4070 (12 GB) and Ollama 0.40.11. Models too big to fit in 12 GB could only run once, so they're left out.

## Results

After the fixes from the first round (below), which shipped in v0.1.3.

| Model | Score | Range over the 3 runs | Pass / partial / fail | Typical answer | Lookup errors | Video memory |
|---|---:|---:|---:|---:|---:|---:|
| **qwen3.5:4b** (Best overall) | 95.5% | 94.1–97.5% | 166 / 6 / 5 | 6.4 s | 9 | 4.5 GB |
| qwen3.5:9b (90 s limit) | 94.4% | 93.2–96.6% | 165 / 4 / 8 | 8.8 s | 4 | 7.3 GB |
| **bonsai-2:9b** (Fastest) | 94.1% | 93.2–95.8% | 162 / 9 / 6 | 5.4 s | 3 | 4.3 GB |
| qwen3.5:9b | 93.8% | 91.5–95.8% | 162 / 8 / 7 | 9.2 s | 3 | 7.3 GB |
| gemma4:12b | 92.9% | 89.8–94.9% | 162 / 5 / 10 | 5.8 s | 3 | 9.8 GB |
| ornith:9b | 91.8% | 89.0–94.1% | 155 / 15 / 7 | 5.4 s | 6 | 6.1 GB |
| ornith-1.5:9b | 91.2% | 83.9–95.8% | 159 / 5 / 13 | 9.1 s | 13 | 7.3 GB |
| **bonsai-2:4b** (Smallest) | 90.1% | 88.1–92.4% | 152 / 15 / 10 | 3.2 s | 15 | 2.8 GB |
| gemma4:e4b | 88.1% | 87.3–89.0% | 149 / 14 / 14 | 3.9 s | 2 | 4.8 GB |
| nemotron-3.5-bolt:e4b | 86.2% | 84.7–88.1% | 148 / 9 / 20 | 2.4 s | 0 | 8.2 GB |
| qwen3.5:2b | 78.0% | 76.3–80.5% | 114 / 48 / 15 | 10.7 s | 64 | 3.4 GB |
| nemotron-3-nano:4b | 47.5% | 44.9–51.7% | 75 / 18 / 84 | 4.7 s | 7 | 3.4 GB |

`qwen3.5:9b (90 s limit)` is the same model rerun with a 90-second limit per answer instead of 60: three of its answers had run out of time.

## First round

The first round ran 20 models on an earlier version of the app. It found problems in the app and in the grading, which were fixed before the round above, so these scores are lower and don't compare directly. The models below weren't run again.

| Model | Score | Range over the 3 runs | Pass / partial / fail | Typical answer | Lookup errors |
|---|---:|---:|---:|---:|---:|
| bonsai-2:2b | 65.8% | 60.2–71.2% | 96 / 41 / 40 | 2.0 s | 14 |
| ministral-3:8b | 61.3% | 52.5–69.5% | 99 / 19 / 59 | 3.6 s | 3 |
| granite4:3b | 55.9% | 50.0–61.9% | 80 / 38 / 59 | 1.8 s | 27 |
| ministral-3:3b | 52.3% | 51.7–52.5% | 84 / 17 / 76 | 1.4 s | 7 |
| llama3.1:8b | 48.9% | 44.9–50.8% | 61 / 51 / 65 | 2.0 s | 43 |
| deepseek-r1:7b | 46.9% | 39.8–51.7% | 80 / 6 / 91 | 5.6 s | 0 |
| lfm2.5-thinking:1.2b | 46.6% | 44.9–50.0% | 59 / 47 / 71 | 3.4 s | 59 |
| llama3.2:latest | 39.8% | 33.1–43.2% | 42 / 57 / 78 | 1.1 s | 54 |
| phi4-mini:3.8b | 32.8% | 32.2–33.9% | 55 / 6 / 116 | 0.8 s | 0 |
| deepseek-r1:1.5b | 27.1% | 22.9–29.7% | 47 / 2 / 128 | 3.0 s | 0 |

In all, 22 models ran the full set three times.
