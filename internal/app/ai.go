package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"btctax/internal/lots"
	"btctax/internal/ollama"
	"btctax/internal/store"
)

// The assistant: an optional chat backed by the user's own Ollama server.
// The server builds the context (a summary plus the page being looked at),
// runs read-only lookup tools the model asks for, and streams the answer
// to the browser. Prompts and answers are saved as chats in the database
// and never logged.

const (
	maxToolRounds  = 8
	historyTurns   = 12   // earlier messages sent along with a new question
	historyChars   = 4000 // per earlier message
	toolRowCap     = 40
	maxQuestionLen = 8000
)

func (s *Server) aiClient() (*ollama.Client, string, error) {
	url, model := s.st.Setting("aiUrl", ""), s.st.Setting("aiModel", "")
	if url == "" || model == "" {
		return nil, "", errors.New("the assistant isn't set up: add an Ollama address and model in Settings")
	}
	c, err := ollama.New(url)
	return c, model, err
}

// aiStatus tells the UI whether to show the chat button.
func (s *Server) aiStatus(w http.ResponseWriter, r *http.Request) {
	c, model, err := s.aiClient()
	if err != nil {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out := map[string]any{"enabled": true, "model": model, "host": c.Host()}
	if _, err := c.Version(ctx); err != nil {
		out["ok"], out["error"] = false, err.Error()
	} else {
		out["ok"] = true
	}
	writeJSON(w, out)
}

// aiModels lists the models at an address, for Settings.
func (s *Server) aiModels(w http.ResponseWriter, r *http.Request) {
	var in struct{ URL string }
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	c, err := ollama.New(in.URL)
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ver, err := c.Version(ctx)
	if err != nil {
		h := c.Host()
		if h == "localhost" || h == "127.0.0.1" || h == "::1" {
			err = fmt.Errorf("%v. Inside Docker, localhost is this app's own container: use http://host.docker.internal:11434 or this computer's network address", err)
		}
		fail(w, 502, err)
		return
	}
	models, err := c.Models(ctx)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]any{"version": ver, "models": models, "public": c.Public()})
}

// Chats

func chatID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (s *Server) aiChats(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Chats()
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, list)
}

func (s *Server) aiNewChat(w http.ResponseWriter, r *http.Request) {
	id, err := s.st.NewChat("New chat", s.st.Setting("aiModel", ""), time.Now())
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (s *Server) aiGetChat(w http.ResponseWriter, r *http.Request) {
	id, err := chatID(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	c, msgs, err := s.st.Chat(id)
	if errors.Is(err, store.ErrNoChat) {
		fail(w, 404, err)
		return
	} else if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"chat": c, "messages": msgs})
}

func (s *Server) aiRenameChat(w http.ResponseWriter, r *http.Request) {
	id, err := chatID(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var in struct{ Title string }
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	t := clip(strings.TrimSpace(in.Title), 80)
	if t == "" {
		fail(w, 400, errors.New("give the chat a name"))
		return
	}
	if err := s.st.RenameChat(id, t); errors.Is(err, store.ErrNoChat) {
		fail(w, 404, err)
		return
	} else if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) aiDeleteChat(w http.ResponseWriter, r *http.Request) {
	id, err := chatID(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.st.DeleteChat(id); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) aiDeleteChats(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteChats(); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// PageContext is where the user is in the UI when they ask.
type PageContext struct {
	Hash     string `json:"hash"`     // e.g. "review", "tx/<txid>", "tx/rec:<key>"
	Year     int    `json:"year"`     // the Overview's tax year
	Selected string `json:"selected"` // the event picked in Flow, if any
	Focus    string `json:"focus"`    // an item the user asked about directly
}

// Lookup is one tool call, as shown under an answer.
type Lookup struct {
	Name    string         `json:"name"`
	Args    map[string]any `json:"args"`
	Summary string         `json:"summary"`
	Link    string         `json:"link,omitempty"`
	Error   string         `json:"error,omitempty"`
	Chart   *Chart         `json:"chart,omitempty"`
}

type msgData struct {
	Page      *PageContext `json:"page,omitempty"`
	Lookups   []Lookup     `json:"lookups,omitempty"`
	Model     string       `json:"model,omitempty"`
	Error     string       `json:"error,omitempty"`
	Thinking  string       `json:"thinking,omitempty"`  // the model's reasoning, when it thought first
	Actions   []int64      `json:"actions,omitempty"`   // changes the assistant proposed or made
	ThinkSecs float64      `json:"thinkSecs,omitempty"` // how long it reasoned
}

const maxSavedThinking = 20000 // characters of reasoning kept with a saved answer

// stream writes newline-delimited JSON events to the browser.
type stream struct {
	mu sync.Mutex
	w  http.ResponseWriter
	f  http.Flusher
}

func (st *stream) send(v map[string]any) {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, _ := json.Marshal(v)
	st.w.Write(append(b, '\n'))
	if st.f != nil {
		st.f.Flush()
	}
}

var capCache sync.Map // "url|model" -> ollama.Model

func (s *Server) modelCaps(ctx context.Context, c *ollama.Client, url, model string) ollama.Model {
	if v, ok := capCache.Load(url + "|" + model); ok {
		return v.(ollama.Model)
	}
	m, err := c.Show(ctx, model)
	if err != nil {
		return ollama.Model{Name: model, Tools: true}
	}
	capCache.Store(url+"|"+model, m)
	return m
}

func (s *Server) aiSend(w http.ResponseWriter, r *http.Request) {
	id, err := chatID(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var in struct {
		Content string      `json:"content"`
		Page    PageContext `json:"page"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	in.Content = strings.TrimSpace(in.Content)
	if in.Content == "" || len(in.Content) > maxQuestionLen {
		fail(w, 400, errors.New("type a question (up to 8,000 characters)"))
		return
	}
	chat, history, err := s.st.Chat(id)
	if errors.Is(err, store.ErrNoChat) {
		fail(w, 404, err)
		return
	} else if err != nil {
		fail(w, 500, err)
		return
	}
	c, model, err := s.aiClient()
	if err != nil {
		fail(w, 400, err)
		return
	}
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	now := time.Now()
	ud, _ := json.Marshal(msgData{Page: &in.Page})
	if _, err := s.st.AddChatMessage(id, store.ChatMessage{Role: "user", Content: in.Content, Data: string(ud)}, model, now); err != nil {
		fail(w, 500, err)
		return
	}
	if chat.Title == "New chat" && len(history) == 0 {
		s.st.RenameChat(id, titleFrom(in.Content))
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	f, _ := w.(http.Flusher)
	out := &stream{w: w, f: f}
	out.send(map[string]any{"type": "start", "model": model})

	url := s.st.Setting("aiUrl", "")
	caps := s.modelCaps(r.Context(), c, url, model)
	numCtx, _ := strconv.Atoi(s.st.Setting("aiCtx", "16384"))
	if caps.Context > 0 && numCtx > caps.Context {
		numCtx = caps.Context
	}
	msgs := []ollama.Message{{Role: "system", Content: s.systemPrompt(rep, in.Page)}}
	if len(history) > historyTurns {
		history = history[len(history)-historyTurns:]
	}
	for _, m := range history {
		if m.Role == "user" || m.Role == "assistant" {
			msgs = append(msgs, ollama.Message{Role: m.Role, Content: clip(m.Content, historyChars)})
		}
	}
	msgs = append(msgs, ollama.Message{Role: "user", Content: in.Content})
	req := ollama.ChatRequest{Model: model, Options: map[string]any{"num_ctx": numCtx, "temperature": 0.2}, KeepAlive: "15m"}
	if caps.Tools {
		req.Tools = toolSpecs()
	}
	// Thinking models reason before answering when the setting is on
	// (the default); the reasoning streams to a collapsible section.
	thinking := caps.Thinking && s.st.Setting("aiThink", "true") == "true"
	if caps.Thinking {
		req.Think = &thinking
	}

	tools := &toolbox{s: s, rep: rep, chatID: id}
	var lookups []Lookup
	var answer, reasoning strings.Builder
	var failMsg string
	var thinkTime time.Duration
	var thinkStart time.Time
	for round := 0; ; round++ {
		// Out of lookups: one last turn without tools so there is always
		// an answer, never a plan that stops mid-sentence.
		if round == maxToolRounds {
			req.Tools = nil
			msgs = append(msgs, ollama.Message{Role: "system", Content: "You have used all your lookups for this question. Answer the user now from the results above. If something is still missing, say what and suggest a narrower question."})
		}
		req.Messages = msgs
		var text strings.Builder
		reply, err := c.Chat(r.Context(), req, func(d string) {
			if !thinkStart.IsZero() {
				thinkTime += time.Since(thinkStart)
				thinkStart = time.Time{}
			}
			text.WriteString(d)
			out.send(map[string]any{"type": "delta", "text": d})
		}, func(d string) {
			if thinkStart.IsZero() {
				thinkStart = time.Now()
				if reasoning.Len() > 0 {
					reasoning.WriteString("\n\n")
					out.send(map[string]any{"type": "thinking", "text": "\n\n"})
				}
			}
			reasoning.WriteString(d)
			out.send(map[string]any{"type": "thinking", "text": d})
		})
		if !thinkStart.IsZero() { // reasoned, then called a tool or stopped
			thinkTime += time.Since(thinkStart)
			thinkStart = time.Time{}
		}
		if err != nil {
			if errors.Is(r.Context().Err(), context.Canceled) {
				failMsg = "stopped"
			} else {
				failMsg = err.Error()
				out.send(map[string]any{"type": "error", "error": failMsg})
			}
			answer.WriteString(text.String())
			break
		}
		answer.WriteString(reply.Content)
		if len(reply.ToolCalls) == 0 || round >= maxToolRounds {
			break
		}
		msgs = append(msgs, reply)
		for _, tc := range reply.ToolCalls {
			had := len(tools.actions)
			lk, result := tools.run(tc.Function.Name, tc.Function.Arguments)
			lookups = append(lookups, lk)
			out.send(map[string]any{"type": "tool", "lookup": lk})
			for _, aid := range tools.actions[had:] {
				if a, err := s.st.Action(aid); err == nil {
					out.send(map[string]any{"type": "action", "action": s.viewActions([]store.Action{a})[0]})
				}
			}
			msgs = append(msgs, ollama.Message{Role: "tool", ToolName: tc.Function.Name, Content: result})
		}
		if answer.Len() > 0 {
			answer.WriteString("\n\n")
			out.send(map[string]any{"type": "delta", "text": "\n\n"})
		}
	}
	text := strings.TrimSpace(answer.String())
	if text == "" && failMsg == "" {
		text = "(The model didn't answer. Try asking again, or pick a larger model in Settings.)"
		out.send(map[string]any{"type": "delta", "text": text})
	}
	md := msgData{Lookups: lookups, Model: model, Error: failMsg, Actions: tools.actions}
	if reasoning.Len() > 0 {
		md.Thinking = clip(strings.TrimSpace(reasoning.String()), maxSavedThinking)
		md.ThinkSecs = float64(thinkTime.Round(100*time.Millisecond)) / float64(time.Second)
	}
	ad, _ := json.Marshal(md)
	// Save even if the browser went away mid-answer.
	mid, _ := s.st.AddChatMessage(id, store.ChatMessage{Role: "assistant", Content: text, Data: string(ad)}, model, time.Now())
	out.send(map[string]any{"type": "done", "id": mid})
}

func titleFrom(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	return clip(q, 60)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Formatting helpers for what the model reads.

func usdStr(c int64) string {
	neg := c < 0
	if neg {
		c = -c
	}
	whole := strconv.FormatInt(c/100, 10)
	var b strings.Builder
	for i, ch := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	s := fmt.Sprintf("$%s.%02d", b.String(), c%100)
	if neg {
		return "-" + s
	}
	return s
}

func btcStr(s int64) string {
	if s < 0 {
		return "-" + satsStr(-s) + " BTC"
	}
	return satsStr(s) + " BTC"
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// pageName turns the UI's location into a readable page and its detail.
func pageName(hash string) (page, arg string) {
	page, arg, _ = strings.Cut(hash, "/")
	if page == "" {
		page = "overview"
	}
	return page, arg
}

func (s *Server) systemPrompt(rep *Report, pc PageContext) string {
	var b strings.Builder
	now := time.Now().UTC()
	fmt.Fprintf(&b, `You are the assistant inside BTC Tax, a self-hosted Bitcoin tax calculator. You help the owner understand their own bitcoin history, cost basis and taxes. Today is %s. Unless the page says otherwise, "this year" means the current calendar year.

Rules:
- Answer from the data below and from your lookup tools. Never invent transactions, amounts or dates. If something isn't in the data, say so.
- The summary below has only totals. For anything about specific sales, purchases, transactions, lots or the biggest/smallest of something, call a tool first and answer from its result. Never derive individual records from totals. Tools are read-only; you cannot change anything in the app.
- Be concise and concrete: short paragraphs or bullet lists, figures with units (BTC to 8 decimals, USD with cents), dates as YYYY-MM-DD.
- To refer to a record, give its date, account and amount in plain words. Don't paste record ids (the [ex:..] or [tx:..] tags) or long txids into answers unless the user asks; the app already links your lookups.
- You are not a tax advisor. Only when the user asks what they should do (sell, harvest losses, how to file), explain the numbers and trade-offs and add one short line suggesting a tax professional. Don't add disclaimers to factual answers.
- Don't narrate that you are about to look something up; just call the tool, then answer.
- For searches, totals, counts, averages, medians, most common values, largest/smallest, "which has the most", lots turning long-term or at a loss, income, Review items and Lightning activity, call analyze_records. Set export=true when the user wants a file, chart=true when a chart would help (with group_by or a histogram). For any arithmetic (sums, differences, comparing gains or losses), call calculate rather than working it out yourself. Remember a loss is a negative gain: -$76 is larger than -$81.
- For one record (details, where its coins came from or went, possible matches), call get_record. For accounts, holdings on a date, wallets and imported files, call accounts.
- For selling: simulate_sale. "Sell everything" = account "all" with all=true (one call). A whole account = all=true. "How much can I sell and stay under $X gain/tax" = target_gain or target_tax.
- To undo a change ("undo that"), call undo_action.
- To point the user to a page, write a markdown link. The pages: [Overview](#overview) (portfolio value, BTC held over time, where coins came from and went, accounts), [Wallets](#wallets) (add and scan wallets), [Imports](#imports) (exchange files), [Review](#review) (items to label), [Flow](#graph) (coins moving between accounts), [Taxes](#taxes) (each year's gains and tax, Form 8949 export, protected purchases, every sale), [Activity](#activity) (every change, with undo), [Settings](#settings), [Lightning](#lightning). A transaction: [text](#tx/TXID); a record without a txid: [text](#tx/rec:RECORD_ID). Never link to anything else, never write placeholder links like (#), and never mention tool names to the user.
- Downloads and charts from your tools appear by themselves under your answer; just say they're there.
- Use account names exactly as listed under Accounts (for example "Cold storage", not "Cold storage (wallet)").
- When "largest", "biggest" or "most" could mean dollars or bitcoin, say which one you used (and give the other if it differs).
- Never write a plan or a heading and stop. Call the tools you need first, then write one complete answer.
- You can propose changes with label_items, link_transfer and protect_lot, but only when the user asks you to change something or agrees to your suggestion. Each proposal appears as a card the user must Apply; never say a change is done unless the tool result says it was applied. Look the items up first so the ids and categories are right.
- Short-term means held one year or less; long-term means more than a year (US rules).

`, now.Format("2006-01-02"))

	set := rep.Settings
	fmt.Fprintf(&b, "## Settings\nJurisdiction %s. Cost basis method %s. Network fees on own transfers: %s. Tax rates used for estimates: short-term %s%%, long-term %s%%.\n\n",
		set["jurisdiction"], strings.ToUpper(set["method"]), map[string]string{"carry": "added to basis", "dispose": "treated as small sales"}[set["feeMode"]], set["rateShort"], set["rateLong"])

	fmt.Fprintf(&b, "## Portfolio now\nHolding %s.", btcStr(rep.Holdings))
	if rep.Price > 0 {
		worth := rep.Holdings * rep.Price / 100_000_000
		fmt.Fprintf(&b, " BTC price %s (as of %s), worth %s, unrealized gain %s.", usdStr(rep.Price), rep.PriceDay, usdStr(worth), usdStr(rep.Unrealized))
	}
	var basis int64
	for _, a := range rep.Accounts {
		basis += a.Basis
	}
	fmt.Fprintf(&b, " Total cost basis of coins held %s.\n\n", usdStr(basis))

	b.WriteString("## Accounts\n")
	for _, a := range rep.Accounts {
		fmt.Fprintf(&b, "- %s: %s holding %s, cost basis %s", a.Name, map[string]string{"wallet": "a wallet", "exchange": "an exchange account"}[a.Kind], btcStr(a.Sats), usdStr(a.Basis))
		if a.ChainSats != nil && *a.ChainSats != a.Sats {
			fmt.Fprintf(&b, "; on-chain balance %s", btcStr(*a.ChainSats))
		}
		if a.PendingOut > 0 || a.PendingIn > 0 {
			fmt.Fprintf(&b, "; awaiting labels: %s out, %s in", btcStr(a.PendingOut), btcStr(a.PendingIn))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n## Tax years\n")
	for _, y := range rep.Years {
		fmt.Fprintf(&b, "- %d: short-term gain %s, long-term gain %s, income %s, estimated tax %s", y.Year,
			usdStr(y.ShortProceeds-y.ShortBasis), usdStr(y.LongProceeds-y.LongBasis), usdStr(y.Income), usdStr(y.EstimatedTax))
		if y.MissingBasis > 0 {
			fmt.Fprintf(&b, "; %s sold without a known cost basis", btcStr(y.MissingBasis))
		}
		b.WriteString("\n")
	}
	if len(rep.Years) == 0 {
		b.WriteString("No sales or income yet.\n")
	}
	fmt.Fprintf(&b, "\n%d items are waiting in Review for a label.", len(rep.Unmatched))
	if len(rep.Warnings) > 0 {
		b.WriteString(" Warnings:")
		for i, w := range rep.Warnings {
			if i >= 5 {
				fmt.Fprintf(&b, " (and %d more)", len(rep.Warnings)-5)
				break
			}
			b.WriteString(" " + w + ".")
		}
	}
	b.WriteString("\n\n")
	b.WriteString(s.pageSlice(rep, pc))
	return b.String()
}

// pageSlice describes what's on the user's screen.
func (s *Server) pageSlice(rep *Report, pc PageContext) string {
	var b strings.Builder
	page, arg := pageName(pc.Hash)
	names := map[string]string{"overview": "Overview", "wallets": "Wallets", "imports": "Imports", "review": "Review",
		"lightning": "Lightning", "graph": "Flow", "taxes": "Taxes", "settings": "Settings", "tx": "transaction", "addr": "address"}
	name := names[page]
	if name == "" {
		name = page
	}
	fmt.Fprintf(&b, "## What the user is looking at\nThe %s page. Questions like \"this\" or \"here\" refer to it.\n", name)
	switch page {
	case "overview":
		yr := pc.Year
		if yr == 0 {
			yr = time.Now().UTC().Year()
		}
		fmt.Fprintf(&b, "The tax year picker is set to %d, so \"this year\" means %[1]d unless the user says otherwise. The page shows portfolio value, %[1]d realized gain and estimated tax, BTC held over time with the price and buy/sell dots, a summary of how coins arrived and left, and account balances.\n", yr)
		b.WriteString(holdingsByYear(rep, yr))
	case "review":
		fmt.Fprintf(&b, "Items that couldn't be matched to another of the user's accounts, waiting for a label (%d):\n", len(rep.Unmatched))
		for i, it := range rep.Unmatched {
			if i >= 25 {
				fmt.Fprintf(&b, "(%d more; use analyze_records with source review and stat list)\n", len(rep.Unmatched)-25)
				break
			}
			b.WriteString("- " + itemLine(it.Key, it.Time, it.Account, string(it.Dir), it.Sats, it.FeeSats, it.Value, it.Network, it.Note, it.Txid) + "\n")
		}
	case "taxes":
		b.WriteString("The page lists each tax year's gains, income and estimated tax (above), Form 8949 exports, protected purchases, and every sale with its lots. Use get_tax_year for a year's sales.\n")
	case "graph":
		b.WriteString("The Flow page shows coins moving between accounts over time.")
		if pc.Selected != "" {
			b.WriteString(" The user has selected this record:\n")
			b.WriteString(eventDetail(rep, pc.Selected))
		}
		b.WriteString("\n")
	case "wallets":
		b.WriteString("Wallets added (watch-only, by public key):\n")
		for _, a := range rep.Accounts {
			if a.Kind == "wallet" {
				fmt.Fprintf(&b, "- %s, script %s, last scanned %s\n", a.Name, a.Script, a.ScannedAt)
			}
		}
	case "imports":
		if imps, err := s.st.Imports(); err == nil {
			b.WriteString("Imported exchange files:\n")
			for i, im := range imps {
				if i >= 30 {
					break
				}
				js, _ := json.Marshal(im)
				b.WriteString("- " + string(js) + "\n")
			}
		}
	case "lightning":
		if ln := rep.Lightning; ln != nil {
			fmt.Fprintf(&b, "Lightning node %s, balance %s, %d channels, %d history events, last synced %s.\n", ln.Account, btcStr(ln.Balance), len(ln.Channels), len(ln.Events), ln.Synced)
		}
	case "settings":
		b.WriteString("The Settings page: node connection, tax method and rates, price source, Lightning node, the assistant, display options, security, backup and restore.\n")
	case "tx":
		key := arg
		if strings.HasPrefix(key, "rec:") {
			key = strings.TrimPrefix(key, "rec:")
			if k, err := urlUnescape(key); err == nil {
				key = k
			}
		}
		b.WriteString("The user is looking at this record:\n")
		b.WriteString(eventDetail(rep, key))
	case "addr":
		fmt.Fprintf(&b, "The address %s.\n", arg)
		n := 0
		for _, g := range rep.Graph {
			if g.Address == arg {
				b.WriteString("- " + graphLine(g) + "\n")
				n++
			}
		}
		if n == 0 {
			b.WriteString("No records in the app use this address.\n")
		}
	}
	if pc.Focus != "" {
		b.WriteString("\nThe user is asking about this specific record:\n")
		b.WriteString(eventDetail(rep, pc.Focus))
	}
	return b.String()
}

func itemLine(key string, t time.Time, acct, dir string, sats, fee, value int64, network, note, txid string) string {
	s := fmt.Sprintf("[%s] %s %s %s %s", key, dayOf(t), acct, dir, btcStr(sats))
	if fee > 0 {
		s += ", fee " + btcStr(fee)
	}
	if value > 0 {
		s += ", worth about " + usdStr(value)
	}
	if network != "" {
		s += ", " + network
	}
	if txid != "" {
		s += ", txid " + txid
	}
	if note != "" {
		s += ", note: " + clip(note, 120)
	}
	return s
}

func graphLine(g GraphEvent) string {
	s := fmt.Sprintf("[%s] %s %s %s %s", g.Key, dayOf(time.Unix(g.Time, 0)), g.Account, kindName(g.Kind), btcStr(g.Sats))
	if g.To != "" {
		s += " to " + g.To
	}
	if g.Cents != 0 {
		s += ", " + usdStr(g.Cents)
	}
	if g.FeeSats > 0 {
		s += ", fee " + btcStr(g.FeeSats)
	}
	if g.Label != "" {
		s += ", label: " + g.Label
	}
	if g.Txid != "" {
		s += ", txid " + g.Txid
	}
	if g.Note != "" {
		s += ", note: " + clip(g.Note, 120)
	}
	return s
}

// kindMatches accepts the words people (and models) use for a kind:
// "sold" or "sale" finds exchange sales and sales to people alike.
func kindMatches(want, have string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == have || kindName(have) == want {
		return true
	}
	groups := [][]string{
		{"sell", "sold", "sale", "sales", "selling", "sells"},
		{"buy", "purchase", "bought", "buys", "purchases", "buying"},
		{"spend", "spent", "payment", "spends", "payments"},
		{"income", "earned", "reward", "rewards", "interest"},
		{"transfer", "moved", "move", "transfers"},
		{"unmatched", "unlabeled", "review"},
		{"gift-out", "gift", "gifted", "gift given"},
		{"gift-in", "gift received"},
	}
	for _, g := range groups {
		in := func(s string) bool {
			for _, x := range g {
				if x == s {
					return true
				}
			}
			return false
		}
		if in(want) && (in(have) || have == "sold" && g[0] == "sell" || have == "purchase" && g[0] == "buy") {
			return true
		}
	}
	return false
}

func kindName(k string) string {
	if n, ok := map[string]string{"buy": "bought", "sell": "sold", "income": "income", "transfer": "moved", "unmatched": "unlabeled",
		"spend": "spent", "sold": "sold to a person", "purchase": "bought from a person", "gift-in": "gift received", "gift-out": "gift given",
		"donation": "donated", "lost": "lost", "refund": "refund", "untracked": "to/from an untracked wallet"}[k]; ok {
		return n
	}
	return k
}

// eventDetail describes one record: the event, its sale lots, its match.
func eventDetail(rep *Report, key string) string {
	var b strings.Builder
	found := false
	for _, g := range rep.Graph {
		if sameRecord(g.Key, g.Txid, key) {
			b.WriteString("- " + graphLine(g) + "\n")
			found = true
		}
	}
	for _, it := range rep.Unmatched {
		if sameRecord(it.Key, it.Txid, key) {
			b.WriteString("- Waiting in Review: " + itemLine(it.Key, it.Time, it.Account, string(it.Dir), it.Sats, it.FeeSats, it.Value, it.Network, it.Note, it.Txid) + "\n")
			found = true
		}
	}
	lots := saleLots(rep, key)
	if len(lots) > 0 {
		b.WriteString("  Lots used by this sale:\n")
		for _, l := range lots {
			b.WriteString("  - " + l + "\n")
		}
	}
	if !found {
		b.WriteString("- (no record with that id or txid)\n")
	}
	return b.String()
}

func saleLots(rep *Report, key string) []string {
	var out []string
	for _, d := range rep.Disposals {
		if d.OpID != key && !strings.HasPrefix(d.OpID, key+":") && !strings.HasPrefix(d.OpID, key+">") {
			continue
		}
		term := "short-term"
		if d.LongTerm {
			term = "long-term"
		}
		s := fmt.Sprintf("%s acquired %s (%s), sold %s: proceeds %s, basis %s, gain %s, %s, from purchase [%s]",
			btcStr(d.Sats), dayOf(d.Acquired), d.Account, dayOf(d.Disposed), usdStr(d.Proceeds), usdStr(d.Basis), usdStr(d.Gain()), term, d.OriginID)
		if d.Missing {
			s += " (no cost basis found: more was sold than the app knows was bought)"
		}
		out = append(out, s)
	}
	return out
}

// sameRecord matches a record by its id or txid, forgiving the ways a
// model shortens them: brackets, a dropped output index, a bare txid.
func sameRecord(key, txid, id string) bool {
	id = strings.Trim(strings.TrimSpace(id), "[]`\"' ")
	if id == "" {
		return false
	}
	if id == key || txid != "" && id == txid {
		return true
	}
	if strings.HasPrefix(id, "tx:") {
		if parts := strings.Split(id, ":"); len(parts) >= 2 && len(parts[1]) == 64 {
			t := parts[1]
			if len(parts) == 2 {
				return txid == t || strings.HasPrefix(key, "tx:"+t+":")
			}
		}
	}
	return len(id) == 64 && strings.HasPrefix(key, "tx:"+id+":")
}

func urlUnescape(s string) (string, error) {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return s, err
			}
			b.WriteByte(byte(v))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

// Tools: read-only lookups over the same report the UI shows.

type toolbox struct {
	s       *Server
	rep     *Report
	chatID  int64
	actions []int64 // changes proposed or applied during this answer
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }

func baseToolSpecs() []ollama.Tool {
	t := func(name, desc string, params map[string]any) ollama.Tool {
		return ollama.Tool{Type: "function", Function: ollama.ToolSpec{Name: name, Description: desc, Parameters: params}}
	}
	return []ollama.Tool{
		t("search_transactions", "Find records (buys, sales, transfers, income, spends, gifts, unlabeled items) by date range, account, kind, amount or text. Returns up to 40, newest first.", obj(map[string]any{
			"from":    str("first day, YYYY-MM-DD"),
			"to":      str("last day, YYYY-MM-DD"),
			"account": str("account name, as listed in Accounts"),
			"kind":    str("one of: buy, sell, income, transfer, unmatched, spend, sold, purchase, gift-in, gift-out, donation, lost, refund, untracked"),
			"min_btc": num("smallest amount in BTC"),
			"max_btc": num("largest amount in BTC"),
			"text":    str("text to match in the note, label, txid or address"),
		})),
		t("get_transaction", "Everything about one record: amounts, fee, label, where it went, the lots a sale used, and whether it waits in Review. Pass the record id in brackets (e.g. ex:12) or an on-chain txid.", obj(map[string]any{
			"id": str("record id or txid"),
		}, "id")),
		t("get_tax_year", "A tax year's totals and its sales (largest gains and losses first), with short/long term and income.", obj(map[string]any{
			"year": num("the year, e.g. 2024"),
		}, "year")),
		t("get_account", "One account: balance, cost basis, the purchase lots it still holds, and its latest records.", obj(map[string]any{
			"name": str("account name"),
		}, "name")),
		t("list_review_items", "Items waiting for a label in Review, with why they couldn't be matched.", obj(map[string]any{
			"account": str("only this account (optional)"),
		})),
		t("price_on", "The BTC price in USD on a day.", obj(map[string]any{
			"date": str("YYYY-MM-DD"),
		}, "date")),
		t("what_if_sale", "Estimate selling bitcoin today from one account: proceeds, cost basis used, gain, short vs long term, and the extra tax for this year. Give btc, usd, or all=true for the whole account.", obj(map[string]any{
			"account": str("account to sell from"),
			"btc":     num("amount in BTC"),
			"usd":     num("amount in USD"),
			"all":     map[string]any{"type": "boolean", "description": "sell everything in this account"},
		}, "account")),
		t("what_if_sell_everything", "Estimate selling ALL bitcoin in every account today, together: total proceeds, cost basis, short- and long-term gain, the extra tax for this year, and a breakdown by account.", obj(map[string]any{})),
	}
}

func argStr(a map[string]any, k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func argNum(a map[string]any, k string) float64 {
	switch v := a[k].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	}
	return 0
}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

// run executes one tool and returns what to show and what the model reads.
func (tb *toolbox) run(name string, args map[string]any) (Lookup, string) {
	if args == nil {
		args = map[string]any{}
	}
	lk := Lookup{Name: name, Args: args}
	var out string
	var err error
	if o, ok, e := tb.runConsolidated(name, args, &lk); ok {
		out, err = o, e
		name = "" // handled
	}
	switch name {
	case "":
	case "search_transactions":
		out, lk.Summary, err = tb.search(args)
	case "get_transaction":
		id := strings.Trim(argStr(args, "id"), "[] ")
		lk.Summary, lk.Link = "Looked up "+recordName(tb.rep, id), "#tx/"+linkArg(tb.rep, id)
		out = eventDetail(tb.rep, id)
	case "get_tax_year":
		out, lk.Summary, err = tb.taxYear(int(argNum(args, "year")))
		lk.Link = "#taxes"
	case "get_account":
		out, lk.Summary, err = tb.account(argStr(args, "name"))
		lk.Link = "#overview"
	case "list_review_items":
		out, lk.Summary = tb.review(argStr(args, "account"))
		lk.Link = "#review"
	case "price_on":
		out, lk.Summary, err = tb.price(argStr(args, "date"))
	case "what_if_sale":
		out, lk.Summary, err = tb.whatIf(args)
	case "what_if_sell_everything":
		out, lk.Summary, err = tb.sellEverything()
	default:
		var ok bool
		if out, ok, err = tb.runMore(name, args, &lk); !ok {
			if out, ok, err = tb.propose(name, args, &lk); !ok {
				err = fmt.Errorf("there is no tool called %q", name)
			}
		}
	}
	if err != nil {
		lk.Error = err.Error()
		if lk.Summary == "" {
			lk.Summary = "Lookup failed"
		}
		return lk, "Error: " + err.Error()
	}
	return lk, out
}

// recordName is a short human name for a record: date, account, amount.
func recordName(rep *Report, id string) string {
	for _, g := range rep.Graph {
		if sameRecord(g.Key, g.Txid, id) {
			return fmt.Sprintf("%s %s %s", dayOf(time.Unix(g.Time, 0)), g.Account, btcStr(g.Sats))
		}
	}
	for _, it := range rep.Unmatched {
		if sameRecord(it.Key, it.Txid, id) {
			return fmt.Sprintf("%s %s %s", dayOf(it.Time), it.Account, btcStr(it.Sats))
		}
	}
	return "record " + clip(id, 20)
}

func linkArg(rep *Report, id string) string {
	for _, g := range rep.Graph {
		if sameRecord(g.Key, g.Txid, id) && g.Txid != "" {
			return g.Txid
		}
	}
	for _, it := range rep.Unmatched {
		if sameRecord(it.Key, it.Txid, id) && it.Txid != "" {
			return it.Txid
		}
	}
	if parts := strings.Split(id, ":"); len(parts) >= 2 && parts[0] == "tx" && len(parts[1]) == 64 {
		return parts[1]
	}
	if len(id) == 64 && !strings.Contains(id, ":") {
		return id
	}
	return "rec:" + strings.ReplaceAll(id, ":", "%3A")
}

func (tb *toolbox) search(a map[string]any) (string, string, error) {
	from, hasFrom := parseDay(argStr(a, "from"))
	to, hasTo := parseDay(argStr(a, "to"))
	acct, kind, text := strings.ToLower(argStr(a, "account")), strings.ToLower(argStr(a, "kind")), strings.ToLower(argStr(a, "text"))
	minS, maxS := int64(argNum(a, "min_btc")*1e8+0.5), int64(argNum(a, "max_btc")*1e8+0.5)
	var hits []GraphEvent
	for _, g := range tb.rep.Graph {
		t := time.Unix(g.Time, 0).UTC()
		if hasFrom && t.Before(from) || hasTo && t.After(to.Add(24*time.Hour-time.Second)) {
			continue
		}
		if acct != "" && !strings.Contains(strings.ToLower(g.Account), acct) && !strings.Contains(strings.ToLower(g.To), acct) {
			continue
		}
		if kind != "" && !kindMatches(kind, g.Kind) {
			continue
		}
		if minS > 0 && g.Sats < minS || maxS > 0 && g.Sats > maxS {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(g.Note+" "+g.Label+" "+g.Txid+" "+g.Address+" "+g.Key), text) {
			continue
		}
		hits = append(hits, g)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Time > hits[j].Time })
	var b strings.Builder
	fmt.Fprintf(&b, "%d records match.", len(hits))
	if len(hits) > toolRowCap {
		fmt.Fprintf(&b, " Showing the newest %d; narrow the search for more.", toolRowCap)
		hits = hits[:toolRowCap]
	}
	b.WriteString("\n")
	for _, g := range hits {
		b.WriteString("- " + graphLine(g) + "\n")
	}
	sum := fmt.Sprintf("Searched records: %d found", len(hits))
	return b.String(), sum, nil
}

func (tb *toolbox) taxYear(y int) (string, string, error) {
	if y < 2009 || y > 2100 {
		return "", "", errors.New("give a year like 2024")
	}
	var b strings.Builder
	var ys *YearSummary
	for i := range tb.rep.Years {
		if tb.rep.Years[i].Year == y {
			ys = &tb.rep.Years[i]
		}
	}
	sum := fmt.Sprintf("Looked up tax year %d", y)
	if ys == nil {
		return fmt.Sprintf("No sales or income in %d.", y), sum, nil
	}
	fmt.Fprintf(&b, "%d: short-term proceeds %s, basis %s, gain %s; long-term proceeds %s, basis %s, gain %s; income %s; estimated tax %s.\n",
		y, usdStr(ys.ShortProceeds), usdStr(ys.ShortBasis), usdStr(ys.ShortProceeds-ys.ShortBasis),
		usdStr(ys.LongProceeds), usdStr(ys.LongBasis), usdStr(ys.LongProceeds-ys.LongBasis), usdStr(ys.Income), usdStr(ys.EstimatedTax))
	type sale struct {
		op                    string
		t                     time.Time
		acct                  string
		sats, proceeds, basis int64
		long, missing         bool
	}
	by := map[string]*sale{}
	var order []string
	for _, d := range tb.rep.Disposals {
		if d.Disposed.UTC().Year() != y {
			continue
		}
		k := fmt.Sprintf("%s|%t", d.OpID, d.LongTerm)
		sl := by[k]
		if sl == nil {
			sl = &sale{op: d.OpID, t: d.Disposed, acct: d.Account, long: d.LongTerm}
			by[k] = sl
			order = append(order, k)
		}
		sl.sats += d.Sats
		sl.proceeds += d.Proceeds
		sl.basis += d.Basis
		sl.missing = sl.missing || d.Missing
	}
	sort.SliceStable(order, func(i, j int) bool {
		gi, gj := by[order[i]].proceeds-by[order[i]].basis, by[order[j]].proceeds-by[order[j]].basis
		if gi < 0 {
			gi = -gi
		}
		if gj < 0 {
			gj = -gj
		}
		return gi > gj
	})
	fmt.Fprintf(&b, "%d sales (grouped by sale and term), largest gains or losses first:\n", len(order))
	for i, k := range order {
		if i >= toolRowCap {
			fmt.Fprintf(&b, "(%d more)\n", len(order)-toolRowCap)
			break
		}
		sl := by[k]
		term := "short-term"
		if sl.long {
			term = "long-term"
		}
		fmt.Fprintf(&b, "- [%s] %s %s sold %s: proceeds %s, basis %s, gain %s, %s", sl.op, dayOf(sl.t), sl.acct, btcStr(sl.sats), usdStr(sl.proceeds), usdStr(sl.basis), usdStr(sl.proceeds-sl.basis), term)
		if sl.missing {
			b.WriteString(", partly without cost basis")
		}
		b.WriteString("\n")
	}
	var inc int64
	n := 0
	for _, r := range tb.rep.Income {
		if r.Time.UTC().Year() == y {
			inc += r.Cents
			n++
		}
	}
	if n > 0 {
		fmt.Fprintf(&b, "%d income records totaling %s.\n", n, usdStr(inc))
	}
	return b.String(), sum, nil
}

func (tb *toolbox) account(name string) (string, string, error) {
	if n, err := tb.resolveAccount(name); err == nil {
		name = n
	}
	var a *AccountView
	for i := range tb.rep.Accounts {
		if strings.EqualFold(tb.rep.Accounts[i].Name, name) {
			a = &tb.rep.Accounts[i]
		}
	}
	if a == nil {
		var names []string
		for _, x := range tb.rep.Accounts {
			names = append(names, x.Name)
		}
		return "", "", fmt.Errorf("no account called %q; accounts are: %s", name, strings.Join(names, ", "))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s): %s, cost basis %s.\n", a.Name, a.Kind, btcStr(a.Sats), usdStr(a.Basis))
	b.WriteString("Purchase lots it still holds, oldest first:\n")
	n := 0
	for _, l := range tb.rep.OpenLots {
		if l.Account != a.Name || l.Sats <= 0 {
			continue
		}
		if n++; n > toolRowCap {
			b.WriteString("(more)\n")
			break
		}
		term := "short-term if sold today"
		if time.Since(l.Acquired) > 365*24*time.Hour {
			term = "long-term if sold today"
		}
		fmt.Fprintf(&b, "- lot %s: %s acquired %s, basis %s (%s per BTC), %s, from [%s]\n", l.ID, btcStr(l.Sats), dayOf(l.Acquired), usdStr(l.Basis), usdStr(lots.MulDiv(l.Basis, 100_000_000, max(l.Sats, 1))), term, l.OriginID)
	}
	b.WriteString("Latest records:\n")
	var evs []GraphEvent
	for _, g := range tb.rep.Graph {
		if g.Account == a.Name || g.To == a.Name {
			evs = append(evs, g)
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time > evs[j].Time })
	for i, g := range evs {
		if i >= 15 {
			break
		}
		b.WriteString("- " + graphLine(g) + "\n")
	}
	return b.String(), "Looked up account " + a.Name, nil
}

func (tb *toolbox) review(acct string) (string, string) {
	var b strings.Builder
	n := 0
	for _, it := range tb.rep.Unmatched {
		if acct != "" && !strings.EqualFold(it.Account, acct) {
			continue
		}
		if n++; n > toolRowCap {
			b.WriteString("(more)\n")
			break
		}
		b.WriteString("- " + itemLine(it.Key, it.Time, it.Account, string(it.Dir), it.Sats, it.FeeSats, it.Value, it.Network, it.Note, it.Txid) + "\n")
	}
	if n == 0 {
		return "Nothing is waiting for a label.", "Checked Review"
	}
	return "These moved BTC in or out of a tracked account and couldn't be matched to another of the user's accounts, so they need a label (for example a sale, a spend, a gift, or a transfer to a wallet not added yet):\n" + b.String(), fmt.Sprintf("Checked Review: %d items", min(n, toolRowCap))
}

func (tb *toolbox) price(day string) (string, string, error) {
	t, ok := parseDay(day)
	if !ok {
		return "", "", errors.New("give the date as YYYY-MM-DD")
	}
	p, err := tb.s.pipeline()
	if err != nil {
		return "", "", err
	}
	c, ok := p.prices.lookup(t)
	if !ok {
		return "", "", fmt.Errorf("no price for %s", day)
	}
	return fmt.Sprintf("BTC closed at %s on %s.", usdStr(c), day), "Looked up the price on " + day, nil
}

func (tb *toolbox) whatIf(a map[string]any) (string, string, error) {
	acct, err := tb.resolveAccount(argStr(a, "account"))
	if err != nil {
		return "", "", err
	}
	in := map[string]any{"account": acct, "btc": argNum(a, "btc"), "usd": argNum(a, "usd")}
	if all, _ := a["all"].(bool); all || argNum(a, "btc") == 0 && argNum(a, "usd") == 0 && a["strategy"] == nil && a["lots"] == nil {
		for _, x := range tb.rep.Accounts {
			if x.Name == acct {
				if x.Sats <= 0 {
					return fmt.Sprintf("%s holds no bitcoin, so there's nothing to sell.", acct), "Estimated a sale from " + acct, nil
				}
				in["btc"], in["usd"] = float64(x.Sats)/1e8, 0.0
			}
		}
	}
	a["account"] = acct
	if s, _ := a["strategy"].(string); s != "" {
		in["strategy"] = s
	}
	if l, ok := a["lots"].([]string); ok && len(l) > 0 {
		in["lots"] = l
	}
	body, _ := json.Marshal(in)
	req := httptest.NewRequest("POST", "/api/whatif", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	tb.s.whatIf(rec, req)
	if rec.Code != 200 {
		var e struct{ Error string }
		json.Unmarshal(rec.Body.Bytes(), &e)
		return "", "", errors.New(e.Error)
	}
	var r struct {
		Sats     int64  `json:"sats"`
		Price    int64  `json:"price"`
		PriceDay string `json:"priceDay"`
		Proceeds int64  `json:"proceeds"`
		Short    struct {
			Sats, Proceeds, Basis int64
		} `json:"short"`
		Long struct {
			Sats, Proceeds, Basis int64
		} `json:"long"`
		Missing  int64    `json:"missingSats"`
		ExtraTax int64    `json:"extraTax"`
		Warnings []string `json:"warnings"`
		Method   string   `json:"method"`
	}
	json.Unmarshal(rec.Body.Bytes(), &r)
	var b strings.Builder
	fmt.Fprintf(&b, "Selling %s from %s today at %s (%s), method %s: proceeds %s.\n", btcStr(r.Sats), argStr(a, "account"), usdStr(r.Price), r.PriceDay, strings.ToUpper(r.Method), usdStr(r.Proceeds))
	fmt.Fprintf(&b, "Short-term part: %s, basis %s, gain %s. Long-term part: %s, basis %s, gain %s.\n",
		btcStr(r.Short.Sats), usdStr(r.Short.Basis), usdStr(r.Short.Proceeds-r.Short.Basis), btcStr(r.Long.Sats), usdStr(r.Long.Basis), usdStr(r.Long.Proceeds-r.Long.Basis))
	fmt.Fprintf(&b, "Estimated extra tax this year: %s.", usdStr(r.ExtraTax))
	if r.Missing > 0 {
		fmt.Fprintf(&b, " %s would have no known cost basis.", btcStr(r.Missing))
	}
	for _, w := range r.Warnings {
		b.WriteString(" " + w)
	}
	return b.String(), "Estimated a sale from " + argStr(a, "account"), nil
}

// holdingsByYear is a compact table of how holdings moved each year.
func holdingsByYear(rep *Report, picked int) string {
	type yr struct{ start, end, in, out, bought, sold, income int64 }
	years := map[int]*yr{}
	get := func(y int) *yr {
		if years[y] == nil {
			years[y] = &yr{}
		}
		return years[y]
	}
	var held int64
	lastYear := 0
	for _, h := range rep.History {
		y := time.Unix(h.T, 0).UTC().Year()
		if y != lastYear {
			get(y).start = held
			lastYear = y
		}
		held = h.Sats
		get(y).end = held
	}
	for _, g := range rep.Graph {
		y := get(time.Unix(g.Time, 0).UTC().Year())
		switch g.Kind {
		case "buy", "purchase":
			y.bought += g.Sats
		case "income":
			y.income += g.Sats
		case "sell", "sold", "spend":
			y.sold += g.Sats
		}
	}
	var ys []int
	for y := range years {
		ys = append(ys, y)
	}
	sort.Ints(ys)
	var b strings.Builder
	b.WriteString("Holdings by year (BTC held at the start and end of each year, and what was bought, earned as income, and sold or spent during it):\n")
	for _, y := range ys {
		v := years[y]
		mark := ""
		if y == picked {
			mark = " <- the selected year"
		}
		fmt.Fprintf(&b, "- %d: start %s, end %s (change %s); bought %s, income %s, sold or spent %s%s\n", y, btcStr(v.start), btcStr(v.end), btcStr(v.end-v.start), btcStr(v.bought), btcStr(v.income), btcStr(v.sold), mark)
	}
	return b.String()
}
