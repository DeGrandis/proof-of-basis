package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/lots"
	"github.com/DeGrandis/proof-of-basis/internal/ollama"
	"github.com/DeGrandis/proof-of-basis/internal/store"
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
	if s.demo != nil {
		return nil, "", errors.New("in the demo, the assistant runs on a hosted model")
	}
	url, model := s.st.Setting("aiUrl", ""), s.st.Setting("aiModel", "")
	if url == "" || model == "" {
		return nil, "", errors.New("the assistant isn't set up: add an Ollama address and model in Settings")
	}
	c, err := ollama.New(url)
	return c, model, err
}

// aiStatus tells the UI whether to show the chat button.
func (s *Server) aiStatus(w http.ResponseWriter, r *http.Request) {
	if s.demo != nil {
		if s.demo.AI == nil {
			writeJSON(w, map[string]any{"enabled": false})
			return
		}
		writeJSON(w, map[string]any{"enabled": true, "ok": true, "model": s.demo.AI.Name(), "host": "AWS Bedrock", "demo": true})
		return
	}
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
	File    *FileRef       `json:"file,omitempty"`
	Review  *ReviewSet     `json:"review,omitempty"`
	// Keys are the first records a lookup listed, so a follow-up ("which
	// account was that?") can look one up by its id; Rows are their lines,
	// so it has their facts even without looking.
	Keys []string `json:"keys,omitempty"`
	Rows []string `json:"rows,omitempty"`
}

// ReviewSet is the Review items an answer listed, for the Label control.
type ReviewSet struct {
	Keys []string `json:"keys"`
	Dir  string   `json:"dir"` // in, out or mixed
}

// FileRef is a file the assistant made, shown as a download under the answer.
type FileRef struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Rows int    `json:"rows"`
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
	// The model: the user's Ollama, or the demo's hosted one (which counts
	// against the visitor's allowance).
	var c *ollama.Client
	var llm interface {
		Chat(context.Context, ollama.ChatRequest, func(string), func(string)) (ollama.Message, error)
	}
	var model string
	if s.demo != nil {
		if s.demo.AI == nil {
			fail(w, 503, errors.New("the demo's assistant isn't available right now"))
			return
		}
		if s.demo.Ask != nil {
			if err := s.demo.Ask(); err != nil {
				fail(w, http.StatusTooManyRequests, err)
				return
			}
		}
		llm, model = s.demo.AI, s.demo.AI.Name()
	} else {
		c, model, err = s.aiClient()
		if err != nil {
			fail(w, 400, err)
			return
		}
		llm = c
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

	caps := ollama.Model{Name: model, Tools: true}
	if c != nil {
		caps = s.modelCaps(r.Context(), c, s.st.Setting("aiUrl", ""), model)
	}
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
			content := clip(m.Content, historyChars)
			if m.Role == "assistant" {
				content += lookupNote(m.Data)
			}
			msgs = append(msgs, ollama.Message{Role: m.Role, Content: content})
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

	tools := &toolbox{s: s, rep: rep, chatID: id, historyMatch: matchFromHistory(history), historyKeys: keysFromHistory(history), refersBack: refersBack(in.Content)}
	var lookups []Lookup
	var answer, reasoning strings.Builder
	var failMsg string
	var thinkTime time.Duration
	var thinkStart time.Time
	corrections := 0
	seen := map[string]bool{} // lookups made this turn, by name and arguments
	for round := 0; ; round++ {
		// Out of lookups: one last turn without tools so there is always
		// an answer, never a plan that stops mid-sentence.
		if round == maxToolRounds {
			req.Tools = nil
			msgs = append(msgs, ollama.Message{Role: "system", Content: "You have used all your lookups for this question. Answer the user now from the results above. If something is still missing, say what and suggest a narrower question."})
		}
		req.Messages = msgs
		var text strings.Builder
		reply, err := llm.Chat(r.Context(), req, func(d string) {
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
		if len(reply.ToolCalls) == 0 && req.Tools != nil && len(tools.actions) == 0 && claimsChange(answer.String()) {
			// It said it changed or proposed something but called no tool,
			// so nothing happened. Take the claim back and make it act,
			// spelling out the call from the search behind its last answer.
			answer.Reset()
			out.send(map[string]any{"type": "reset"})
			if (tools.historyMatch != "" || len(tools.historyKeys) > 0) && refersBack(in.Content) && (categoryFromText(in.Content) != "" || tiersFromText(in.Content) != nil) {
				break // the code after the loop makes this proposal exactly
			}
			if corrections >= 2 {
				// The model won't call the tool: make the proposal from the
				// search behind its last answer and the category the user
				// named, or say plainly that nothing happened.
				msg := "I didn't actually make that change: no proposal was created. Use the Label button under my earlier list, or ask again in one sentence, for example \"label the 8 unlabeled 2026 outflows as spends\"."
				if m := matchFromHistory(history); m != "" {
					if cat := categoryFromText(in.Content); cat != "" {
						var match map[string]any
						json.Unmarshal([]byte(m), &match)
						lk, result := tools.run("label_items", map[string]any{"match": match, "category": cat})
						lookups = append(lookups, lk)
						out.send(map[string]any{"type": "tool", "lookup": lk})
						if len(tools.actions) > 0 {
							if a, err := s.st.Action(tools.actions[len(tools.actions)-1]); err == nil {
								out.send(map[string]any{"type": "action", "action": s.viewActions([]store.Action{a})[0]})
								msg = fmt.Sprintf("Here's the proposal: %s from my earlier list as %s. Press Apply on the card to make the change.", strings.ToLower(a.Summary[:1])+a.Summary[1:], cat)
							}
						} else {
							msg = "I couldn't make that proposal: " + strings.TrimPrefix(result, "Error: ")
						}
					}
				}
				answer.WriteString(msg)
				out.send(map[string]any{"type": "delta", "text": msg})
				break
			}
			corrections++
			hint := "Correction: you told the user you proposed or made a change, but you called no tool, so nothing was proposed and no card exists. Call the tool now, in this reply. "
			if m := matchFromHistory(history); m != "" {
				hint += "To label the items from your earlier answer, call label_items with match " + m + " and the category the user gave."
			} else {
				hint += "For items from an earlier answer, call label_items with match using the same filters as that earlier lookup."
			}
			hint += " If you truly can't, say plainly that no change was made."
			msgs = append(msgs, reply, ollama.Message{Role: "system", Content: hint})
			continue
		}
		if len(reply.ToolCalls) == 0 || round >= maxToolRounds {
			break
		}
		msgs = append(msgs, reply)
		for _, tc := range reply.ToolCalls {
			// The same lookup again in one turn adds nothing; models
			// sometimes repeat it until they run out of rounds.
			sig, _ := json.Marshal(tc.Function.Arguments)
			if sig := tc.Function.Name + string(sig); seen[sig] {
				msgs = append(msgs, ollama.Message{Role: "tool", ToolName: tc.Function.Name, Content: "You already made this exact lookup; its result is above. Answer from it."})
				continue
			} else {
				seen[sig] = true
			}
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
	// A clear request to label the items just listed ("those were all
	// spends") that ended without a proposal: make it from that list.
	if failMsg == "" && len(tools.actions) == 0 && (tools.historyMatch != "" || len(tools.historyKeys) > 0) && refersBack(in.Content) {
		tiers := tiersFromText(in.Content)
		cat := categoryFromText(in.Content)
		if tiers != nil || cat != "" {
			args := map[string]any{"category": cat}
			if tiers != nil {
				args = map[string]any{"category": "", "tiers": tiers}
				cat = "split by amount"
			}
			if items := tools.historyItems("x"); items != nil {
				for _, it := range items {
					it.(map[string]any)["category"] = args["category"]
				}
				args["items"] = items
			} else {
				var match map[string]any
				json.Unmarshal([]byte(tools.historyMatch), &match)
				args["match"] = match
			}
			lk, result := tools.run("label_items", args)
			lookups = append(lookups, lk)
			out.send(map[string]any{"type": "tool", "lookup": lk})
			answer.Reset()
			out.send(map[string]any{"type": "reset"})
			msg := "I couldn't make that proposal: " + strings.TrimPrefix(result, "Error: ")
			if len(tools.actions) > 0 {
				if a, err := s.st.Action(tools.actions[len(tools.actions)-1]); err == nil {
					out.send(map[string]any{"type": "action", "action": s.viewActions([]store.Action{a})[0]})
					msg = fmt.Sprintf("Here's the proposal: %s from my earlier list as %s. Press Apply on the card to make the change.", strings.ToLower(a.Summary[:1])+a.Summary[1:], cat)
				}
			}
			answer.WriteString(msg)
			out.send(map[string]any{"type": "delta", "text": msg})
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
	fmt.Fprintf(&b, `You are the assistant inside Proof of Basis, a self-hosted Bitcoin tax calculator. You help the owner understand their own bitcoin history, cost basis and taxes. Today is %s. Unless the page says otherwise, "this year" means the current calendar year.

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
- This app untangles the user's records and prepares documents for their tax professional; it doesn't prepare returns. On [Taxes](#taxes): "Ready for your accountant?" lists what to fix for a year and downloads the accountant package; Form 8949 lines by box, checked against imported 1099-DAs; income by type; gifts and donations; the January 1, 2025 per-wallet split. On [Overview](#overview), "Plan a sale or spend" shows the coins a sale would use and can record that choice before the sale. Point to these when they answer the question.
- To correct a record already labeled, or the value of imported income, use label_items with its id, category and usd.

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
	fmt.Fprintf(&b, " Total cost basis of coins held %s", usdStr(basis))
	if rep.Holdings > 0 {
		fmt.Fprintf(&b, ", an average cost of %s per BTC", usdStr(lots.MulDiv(basis, 100_000_000, rep.Holdings)))
	}
	b.WriteString(".\n\n")

	b.WriteString("## Accounts\n")
	for _, a := range rep.Accounts {
		fmt.Fprintf(&b, "- %s: %s holding %s, cost basis %s", a.Name, map[string]string{"wallet": "a wallet", "exchange": "an exchange account", "lightning": "a Lightning node"}[a.Kind], btcStr(a.Sats), usdStr(a.Basis))
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
	} else {
		var gain, income int64
		for _, y := range rep.Years {
			gain += y.ShortProceeds - y.ShortBasis + y.LongProceeds - y.LongBasis
			income += y.Income
		}
		fmt.Fprintf(&b, "- All years: realized gain %s, income %s\n", usdStr(gain), usdStr(income))
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
	// Say "received/sent, not yet labeled": models read a bare in/out as a
	// buy or a sale.
	verb := map[string]string{"in": "received (not yet labeled)", "out": "sent (not yet labeled)"}[dir]
	if verb == "" {
		verb = dir
	}
	s := fmt.Sprintf("[%s] %s %s %s %s", key, dayOf(t), acct, verb, btcStr(sats))
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
		b.WriteString("  " + saleSummary(rep, key) + "\n")
		b.WriteString("  Lots used by this sale:\n")
		for _, l := range lots {
			b.WriteString("  - " + l + "\n")
		}
	}
	if !found {
		b.WriteString("- (no record with that id or txid. Ids look like ex:123 or tx:<txid>:<n>: use one noted on an earlier answer, or find it with analyze_records, stat list)\n")
	}
	return b.String()
}

func ofSale(opID, key string) bool {
	return opID == key || strings.HasPrefix(opID, key+":") || strings.HasPrefix(opID, key+">")
}

// saleSummary totals a sale's lots: how long the coins were held, and
// its gain by term, so no one has to work it out from the lot list.
func saleSummary(rep *Report, key string) string {
	var sats, proceeds, basis, short, long int64
	var minD, maxD, weighted float64
	n := 0
	for _, d := range rep.Disposals {
		if !ofSale(d.OpID, key) {
			continue
		}
		days := float64(int(d.Disposed.Sub(d.Acquired).Hours() / 24))
		if n == 0 || days < minD {
			minD = days
		}
		maxD = max(maxD, days)
		weighted += days * float64(d.Sats)
		sats, proceeds, basis = sats+d.Sats, proceeds+d.Proceeds, basis+d.Basis
		if d.LongTerm {
			long += d.Gain()
		} else {
			short += d.Gain()
		}
		n++
	}
	held := fmt.Sprintf("%.0f days", minD)
	if maxD > minD {
		held = fmt.Sprintf("%.0f to %.0f days (%.0f on average)", minD, maxD, weighted/float64(max(sats, 1)))
	}
	return fmt.Sprintf("Sale summary: %s from %d lots held %s; proceeds %s, basis %s, gain %s (short-term %s, long-term %s).",
		btcStr(sats), n, held, usdStr(proceeds), usdStr(basis), usdStr(proceeds-basis), usdStr(short), usdStr(long))
}

func saleLots(rep *Report, key string) []string {
	var out []string
	for _, d := range rep.Disposals {
		if !ofSale(d.OpID, key) {
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
	// "529" for "ex:529": a bare record number.
	if _, err := strconv.Atoi(id); err == nil && key == "ex:"+id {
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
	// historyMatch is the Review search behind the previous answer, as
	// label_items match filters, for when the model gets the ids wrong.
	historyMatch string
	refersBack   bool     // the user said "these", "those", "them"…
	historyKeys  []string // the Review rows the previous answer showed
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
		Lots     []struct {
			OriginID string `json:"originId"`
			Acquired int64  `json:"acquired"`
			Sats     int64  `json:"sats"`
			Basis    int64  `json:"basis"`
			Proceeds int64  `json:"proceeds"`
			LongTerm bool   `json:"longTerm"`
			Missing  bool   `json:"missing"`
		} `json:"lots"`
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
	// The lots it would use, so "which lots?" needn't be guessed.
	if len(r.Lots) > 0 {
		fmt.Fprintf(&b, "\nLots it would use (%d):\n", len(r.Lots))
	}
	for i, l := range r.Lots {
		if i == 10 {
			fmt.Fprintf(&b, "(%d more)\n", len(r.Lots)-10)
			break
		}
		term := "short-term"
		if l.LongTerm {
			term = "long-term"
		}
		fmt.Fprintf(&b, "- %s acquired %s, basis %s, proceeds %s, gain %s, %s, from [%s]\n", btcStr(l.Sats), dayOf(time.Unix(l.Acquired, 0)),
			usdStr(l.Basis), usdStr(l.Proceeds), usdStr(l.Proceeds-l.Basis), term, l.OriginID)
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

var claimRe = regexp.MustCompile(`(?i)\b(i have|i've|i)\s+(just\s+|now\s+|successfully\s+|already\s+)*(re-?)?(proposed|applied|labell?ed|linked|protected|undone|submitted|updated)\b` +
	`|\bcard\b[^.]*\b(appear|shown|show up|summari[sz]|in the chat)|\b(press|click|tap|hit)(ing)?\s+(the\s+)?apply\b|\bapply (the )?(labels?|changes?|to confirm|button)\b|waiting for you to (apply|confirm)`)

// claimsChange spots an answer saying a change was proposed or made.
func claimsChange(text string) bool {
	return claimRe.MatchString(strings.NewReplacer("**", "", "__", "", "*", "", "`", "").Replace(text))
}

// lookupNote tells the model which lookups an earlier answer used, so a
// follow-up like "label those" can repeat the same filters.
func lookupNote(data string) string {
	var d msgData
	if json.Unmarshal([]byte(data), &d) != nil || len(d.Lookups) == 0 {
		return ""
	}
	var parts []string
	for i, lk := range d.Lookups {
		if i >= 3 || lk.Error != "" {
			continue
		}
		b, _ := json.Marshal(lk.Args)
		part := lk.Name + " " + clip(string(b), 300)
		if len(lk.Rows) > 0 {
			part += " -> " + strings.Join(lk.Rows, "; ")
		} else if len(lk.Keys) > 0 {
			part += " -> records " + strings.Join(lk.Keys, ", ")
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\n[Lookups behind this answer: " + strings.Join(parts, "; ") + "]"
}

// matchFromHistory turns the Review search behind the latest answer into
// label_items match filters, so a "label those" follow-up can be exact.
func matchFromHistory(history []store.ChatMessage) string {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != "assistant" {
			continue
		}
		var d msgData
		if json.Unmarshal([]byte(m.Data), &d) != nil {
			return ""
		}
		for j := len(d.Lookups) - 1; j >= 0; j-- {
			lk := d.Lookups[j]
			if lk.Name != "analyze_records" || lk.Error != "" || argStr(lk.Args, "source") != "review" {
				continue
			}
			match := map[string]any{}
			for _, k := range []string{"account", "from", "to", "text"} {
				if v := argStr(lk.Args, k); v != "" {
					match[k] = v
				}
			}
			if k := strings.ToLower(argStr(lk.Args, "kind")); k == "in" || k == "out" {
				match["direction"] = k
			}
			if m := argStr(lk.Args, "measure"); m == "" || m == "usd" {
				if _, ok := lk.Args["min"]; ok {
					match["min_usd"] = argNum(lk.Args, "min")
				}
				if _, ok := lk.Args["max"]; ok {
					match["max_usd"] = argNum(lk.Args, "max")
				}
			}
			b, _ := json.Marshal(match)
			return string(b)
		}
		return "" // only the latest answer counts
	}
	return ""
}

// categoryFromText finds the one label category a user's message names.
func categoryFromText(text string) string {
	t := " " + strings.ToLower(text) + " "
	words := map[string][]string{
		"spend":         {" spend", " spent", "purchase of", " paid for", " payment"},
		"gift":          {" gift"},
		"donation":      {" donat"},
		"lost":          {" lost", " stolen"},
		"sold":          {" sold", " sale"},
		"income":        {" income", " earned", " salary", " reward"},
		"self-transfer": {"my own wallet", "my wallet", "self transfer", "self-transfer", "transfer to me"},
	}
	found := ""
	for cat, ws := range words {
		for _, w := range ws {
			if strings.Contains(t, w) {
				if found != "" && found != cat {
					return "" // more than one: don't guess
				}
				found = cat
			}
		}
	}
	return found
}

var backRe = regexp.MustCompile(`(?i)\b(these|those|them|they|all of (these|those|them)|the above|that list|each of them|the ones|that one|this one|that (tx|transaction|record|item|send|payment))\b`)

// refersBack reports whether a message points at items from the previous
// answer.
func refersBack(text string) bool { return backRe.MatchString(text) }

// keysFromHistory are the Review rows the latest answer showed.
func keysFromHistory(history []store.ChatMessage) []string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "assistant" {
			continue
		}
		var d msgData
		if json.Unmarshal([]byte(history[i].Data), &d) != nil {
			return nil
		}
		for j := len(d.Lookups) - 1; j >= 0; j-- {
			if r := d.Lookups[j].Review; r != nil && len(r.Keys) > 0 {
				return r.Keys
			}
		}
		return nil
	}
	return nil
}

var tierRe = regexp.MustCompile(`(?i)(under|below|less than|up to|<)\s*\$?\s*([\d,]+(?:\.\d+)?)\s*(?:dollars)?([^.;]*?)\b(above|over|more|the rest|rest|others|other ones|bigger|larger|higher)\b([^.;]*)`)

// tiersFromText reads "under $25 as spends and the ones above as
// donations" into two value bands.
func tiersFromText(text string) []any {
	m := tierRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	limit, err := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", ""), 64)
	if err != nil {
		return nil
	}
	low, high := categoryFromText(m[3]), categoryFromText(m[5])
	if low == "" || high == "" || low == high {
		return nil
	}
	return []any{map[string]any{"max_usd": limit, "category": low}, map[string]any{"category": high}}
}
