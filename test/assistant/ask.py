"""Ask the public demo's assistant a fixed set of questions and record what
it answered and which lookups it made, to compare models or check a change.

    python test/assistant/ask.py https://proofofbasis.degrand.is out.jsonl

Each conversation in questions.txt starts on a fresh demo copy ("Start
over"), so it stays within the demo's 20 questions per visitor. Every
question counts toward the demo's daily total too. Writes one JSON line per
question and prints the answers as it goes. Standard library only.
"""

import http.cookiejar
import json
import pathlib
import sys
import time
import urllib.request


def conversations(path):
    convs, cur = [], []
    for line in pathlib.Path(path).read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line.startswith("#"):
            continue
        if not line:
            if cur:
                convs.append(cur)
            cur = []
            continue
        cur.append(line)
    if cur:
        convs.append(cur)
    return convs


class Visitor:
    def __init__(self, base):
        self.base = base.rstrip("/")
        self.http = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def call(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, method=method, headers={"Content-Type": "application/json"})
        with self.http.open(req, timeout=300) as r:
            return r.read().decode()

    def fresh(self):
        self.call("POST", "/api/demo/reset", {})
        self.call("GET", "/")
        self.call("POST", "/api/disclaimer", {})
        return json.loads(self.call("POST", "/api/ai/chats", {}))["id"]

    def ask(self, chat, question):
        start = time.time()
        raw = self.call("POST", f"/api/ai/chats/{chat}/send", {"content": question, "page": {"hash": "overview"}})
        answer, lookups, errors, model = "", [], [], ""
        for line in raw.splitlines():
            e = json.loads(line)
            kind = e.get("type")
            if kind == "start":
                model = e.get("model", "")
            elif kind == "delta":
                answer += e["text"]
            elif kind == "reset":
                answer = ""
            elif kind == "tool":
                lk = e["lookup"]
                lookups.append({k: lk.get(k) for k in ("name", "args", "summary", "error") if lk.get(k)})
            elif kind == "error":
                errors.append(e.get("error"))
        return {"question": question, "answer": answer.strip(), "lookups": lookups, "errors": errors,
                "model": model, "seconds": round(time.time() - start, 1)}


def main():
    base = sys.argv[1] if len(sys.argv) > 1 else "https://proofofbasis.degrand.is"
    out = pathlib.Path(sys.argv[2] if len(sys.argv) > 2 else "answers.jsonl")
    questions = pathlib.Path(__file__).with_name("questions.txt")
    v = Visitor(base)
    with out.open("w", encoding="utf-8") as f:
        for n, conv in enumerate(conversations(questions), 1):
            chat = v.fresh()
            for q in conv:
                r = v.ask(chat, q)
                r["conversation"] = n
                f.write(json.dumps(r) + "\n")
                f.flush()
                calls = ", ".join(lk["name"] + ("!" if lk.get("error") else "") for lk in r["lookups"]) or "no lookups"
                print(f"\n[{n}] {q}\n    ({calls}; {r['seconds']}s)\n    " + (r["answer"] or "(no answer) " + "; ".join(map(str, r["errors"]))).replace("\n", "\n    "))


if __name__ == "__main__":
    main()
