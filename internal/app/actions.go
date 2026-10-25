package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"btctax/internal/ollama"
	"btctax/internal/store"
	"btctax/internal/trace"
)

// Every change to the user's data is logged as an action with the state
// before and after, so it can be undone (and redone) from Activity. The
// assistant's changes are proposed first and applied only when the user
// presses Apply, unless Settings says to apply them straight away.

// change is one piece of state an action touched.
//   - label: Key is the record; state is the label's JSON or null.
//   - setting: Key is the setting; state is a JSON string or null (unset).
//   - protect: Key is the purchase record; state is true or false.
//   - rows: Key describes the rows; state is []store.RowSet.
type change struct {
	Kind   string          `json:"kind"`
	Key    string          `json:"key"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
	Sets   []store.RowSet  `json:"sets,omitempty"` // rows: which rows (without contents)
}

var errConflict = errors.New("this was changed again after the action; undoing now would overwrite the later change")

const null = "null"

func (s *Server) stateOf(c change) (json.RawMessage, error) {
	switch c.Kind {
	case "label":
		if raw := s.st.LabelRaw(c.Key); raw != "" {
			return json.RawMessage(raw), nil
		}
		return json.RawMessage(null), nil
	case "setting":
		if v, ok := s.st.SettingRaw(c.Key); ok {
			b, _ := json.Marshal(v)
			return b, nil
		}
		return json.RawMessage(null), nil
	case "protect":
		return json.RawMessage(strconv.FormatBool(s.protected()[c.Key])), nil
	case "rows":
		sets, err := s.st.CaptureRows(c.Sets)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(sets)
		return b, nil
	}
	return nil, fmt.Errorf("unknown change %q", c.Kind)
}

func (s *Server) setState(c change, state json.RawMessage) error {
	isNull := len(state) == 0 || string(state) == null
	switch c.Kind {
	case "label":
		if isNull {
			return s.st.DeleteLabel(c.Key)
		}
		return s.st.SetLabelRaw(c.Key, string(state))
	case "setting":
		if isNull {
			return s.st.DeleteSetting(c.Key)
		}
		var v string
		if err := json.Unmarshal(state, &v); err != nil {
			return err
		}
		return s.st.SetSetting(c.Key, v)
	case "protect":
		return s.setProtected(c.Key, string(state) == "true")
	case "rows":
		var sets []store.RowSet
		dec := json.NewDecoder(strings.NewReader(string(state)))
		dec.UseNumber()
		if err := dec.Decode(&sets); err != nil {
			return err
		}
		return s.st.ReplaceRows(sets)
	}
	return fmt.Errorf("unknown change %q", c.Kind)
}

func (s *Server) setProtected(key string, on bool) error {
	set := s.protected()
	if on {
		set[key] = true
	} else {
		delete(set, key)
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return s.st.SetSetting("protected", strings.Join(ids, "\n"))
}

func sameState(a, b json.RawMessage) bool {
	norm := func(r json.RawMessage) string {
		if len(r) == 0 {
			return null
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(string(r)))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return string(r)
		}
		out, _ := json.Marshal(v)
		return string(out)
	}
	return norm(a) == norm(b)
}

// snapshot reads the current state of each change into Before.
func (s *Server) snapshot(cs []change) ([]change, error) {
	for i := range cs {
		st, err := s.stateOf(cs[i])
		if err != nil {
			return nil, err
		}
		cs[i].Before = st
	}
	return cs, nil
}

// finish reads the state after a change into After and logs the action.
// Changes whose state didn't actually change are dropped; nothing is
// logged if nothing changed.
func (s *Server) finish(actor, kind, summary string, chatID int64, cs []change) (int64, error) {
	var kept []change
	for _, c := range cs {
		st, err := s.stateOf(c)
		if err != nil {
			return 0, err
		}
		c.After = st
		if !sameState(c.Before, c.After) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return 0, nil
	}
	b, _ := json.Marshal(kept)
	return s.st.AddAction(store.Action{Actor: actor, Kind: kind, Summary: summary, Changes: b, Status: "done", ChatID: chatID, Undoable: true})
}

// note logs something that happened but can't be undone from here.
func (s *Server) note(kind, summary string) {
	s.st.AddAction(store.Action{Actor: "user", Kind: kind, Summary: summary, Status: "info"})
}

// record wraps a write: snapshot, do it, then log what changed.
func (s *Server) record(actor, kind, summary string, chatID int64, cs []change, do func() error) (int64, error) {
	cs, err := s.snapshot(cs)
	if err != nil {
		return 0, err
	}
	if err := do(); err != nil {
		return 0, err
	}
	return s.finish(actor, kind, summary, chatID, cs)
}

// Setting keys never logged (secrets) or not worth logging (caches).
var unloggedSettings = map[string]bool{"clnRune": true, "clnChannels": true, "clnSynced": true, "clnAlias": true}

func settingChanges(keys []string) []change {
	var cs []change
	seen := map[string]bool{}
	for _, k := range keys {
		if unloggedSettings[k] || seen[k] {
			continue
		}
		seen[k] = true
		cs = append(cs, change{Kind: "setting", Key: k})
	}
	return cs
}

func accountSets(id int64) []store.RowSet {
	return []store.RowSet{{Table: "accounts", Col: "id", Val: id}, {Table: "imports", Col: "account_id", Val: id},
		{Table: "events", Col: "account_id", Val: id}, {Table: "wallet_txs", Col: "account_id", Val: id}}
}

func importSets(id int64) []store.RowSet {
	return []store.RowSet{{Table: "imports", Col: "id", Val: id}, {Table: "events", Col: "import_id", Val: id}}
}

// --- endpoints ---

// actionView is an action as Activity shows it, with readable lines.
type actionView struct {
	store.Action
	Lines []string `json:"lines"`
}

func (s *Server) viewActions(list []store.Action) []actionView {
	rep, _ := s.buildReport()
	out := make([]actionView, 0, len(list))
	for _, a := range list {
		var cs []change
		json.Unmarshal(a.Changes, &cs)
		v := actionView{Action: a}
		v.Action.Changes = nil
		if a.Actor == "assistant" && a.Status != "pending" && a.Status != "dismissed" {
			v.Summary = pastTense(a.Summary)
		}
		for _, c := range cs {
			v.Lines = append(v.Lines, describeChange(rep, c, a.Status == "pending"))
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) listActions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	actor := r.URL.Query().Get("actor")
	if actor != "" && actor != "user" && actor != "assistant" {
		actor = ""
	}
	list, err := s.st.Actions(actor, limit)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, s.viewActions(list))
}

func (s *Server) getAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.actionFromPath(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, s.viewActions([]store.Action{a})[0])
}

func (s *Server) actionFromPath(r *http.Request) (store.Action, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return store.Action{}, store.ErrNoAction
	}
	return s.st.Action(id)
}

// undoAction puts back the state before; redoAction the state after. With
// force, a later change to the same thing is overwritten.
func (s *Server) undoAction(w http.ResponseWriter, r *http.Request) { s.flipAction(w, r, true) }
func (s *Server) redoAction(w http.ResponseWriter, r *http.Request) { s.flipAction(w, r, false) }

func (s *Server) flipAction(w http.ResponseWriter, r *http.Request, undo bool) {
	var in struct{ Force bool }
	decode(r, &in)
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	a, err := s.actionFromPath(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	want, from, to := "done", "After", "Before"
	if !undo {
		want, from, to = "undone", "Before", "After"
	}
	if !a.Undoable || a.Status != want {
		fail(w, 400, fmt.Errorf("this action is %s and can't be %s", a.Status, map[bool]string{true: "undone", false: "redone"}[undo]))
		return
	}
	var cs []change
	json.Unmarshal(a.Changes, &cs)
	// Added or deleted rows (accounts, imports) aren't conflict-checked:
	// undoing an added wallet removes it however much it was scanned
	// since. The side being replaced is refreshed so redo restores it.
	for i, c := range cs {
		if c.Kind != "rows" {
			continue
		}
		cur, err := s.stateOf(c)
		if err != nil {
			fail(w, 500, err)
			return
		}
		if undo {
			cs[i].After = cur
		} else {
			cs[i].Before = cur
		}
	}
	if !in.Force {
		for _, c := range cs {
			if c.Kind == "rows" {
				continue
			}
			cur, err := s.stateOf(c)
			if err != nil {
				fail(w, 500, err)
				return
			}
			expect := c.After
			if from == "Before" {
				expect = c.Before
			}
			if !sameState(cur, expect) {
				fail(w, http.StatusConflict, errConflict)
				return
			}
		}
	}
	for i := len(cs) - 1; i >= 0; i-- {
		st := cs[i].Before
		if to == "After" {
			st = cs[i].After
		}
		if err := s.setState(cs[i], st); err != nil {
			fail(w, 500, err)
			return
		}
	}
	status := "undone"
	if !undo {
		status = "done"
	}
	nb, _ := json.Marshal(cs)
	if err := s.st.UpdateAction(a.ID, status, nb); err != nil {
		fail(w, 500, err)
		return
	}
	a.Status = status
	writeJSON(w, s.viewActions([]store.Action{a})[0])
}

// applyAction carries out a pending proposal from the assistant.
func (s *Server) applyAction(w http.ResponseWriter, r *http.Request) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	a, err := s.actionFromPath(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	if a.Status != "pending" {
		fail(w, 400, fmt.Errorf("this change is already %s", a.Status))
		return
	}
	if err := s.applyPending(&a); err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, s.viewActions([]store.Action{a})[0])
}

func (s *Server) applyPending(a *store.Action) error {
	var cs []change
	json.Unmarshal(a.Changes, &cs)
	cs, err := s.snapshot(cs)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := s.setState(c, c.After); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(cs)
	a.Changes, a.Status = b, "done"
	return s.st.UpdateAction(a.ID, "done", b)
}

func (s *Server) dismissAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.actionFromPath(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	if a.Status != "pending" {
		fail(w, 400, fmt.Errorf("this change is already %s", a.Status))
		return
	}
	s.st.UpdateAction(a.ID, "dismissed", nil)
	a.Status = "dismissed"
	writeJSON(w, s.viewActions([]store.Action{a})[0])
}

// --- readable descriptions ---

var categoryNames = map[trace.Category]string{
	trace.Spend: "a spend (paid for something)", trace.Sold: "sold to a person", trace.Gift: "a gift sent", trace.Donation: "a donation",
	trace.Lost: "lost or stolen", trace.IncomeIn: "income", trace.GiftIn: "a gift received", trace.PurchaseIn: "bought from a person",
	trace.RefundIn: "a refund", trace.Untracked: "to or from a wallet not added here", trace.SelfLink: "a transfer between my accounts",
	trace.SelfTransfer: "a transfer to or from my own untracked wallet",
}

func labelText(raw json.RawMessage) (trace.Label, bool) {
	var l trace.Label
	if len(raw) == 0 || string(raw) == null || json.Unmarshal(raw, &l) != nil {
		return l, false
	}
	return l, true
}

func describeLabel(rep *Report, l trace.Label) string {
	s := categoryNames[l.Category]
	if s == "" {
		s = string(l.Category)
	}
	if l.Category == trace.SelfLink && l.Counterpart != "" && rep != nil {
		s += " (the other side is " + recordName(rep, l.Counterpart) + ")"
	}
	if l.FiatCents > 0 {
		s += ", " + usdStr(l.FiatCents)
	}
	if l.Note != "" {
		s += ", note: " + clip(l.Note, 60)
	}
	return s
}

func describeChange(rep *Report, c change, pending bool) string {
	name := c.Key
	if rep != nil && (c.Kind == "label" || c.Kind == "protect") {
		name = recordName(rep, c.Key)
	}
	switch c.Kind {
	case "label":
		before, hadB := labelText(c.Before)
		after, hadA := labelText(c.After)
		switch {
		case hadA && hadB:
			return fmt.Sprintf("%s: label changed from %s to %s", name, describeLabel(rep, before), describeLabel(rep, after))
		case hadA:
			if pending {
				return fmt.Sprintf("%s: label as %s", name, describeLabel(rep, after))
			}
			return fmt.Sprintf("%s: labeled as %s", name, describeLabel(rep, after))
		case hadB:
			return fmt.Sprintf("%s: label removed (was %s)", name, describeLabel(rep, before))
		}
	case "protect":
		if string(c.After) == "true" {
			return name + ": protected (sold last)"
		}
		return name + ": no longer protected"
	case "setting":
		var b, a string
		json.Unmarshal(c.Before, &b)
		json.Unmarshal(c.After, &a)
		name := settingNames[c.Key]
		if name == "" {
			name = c.Key
		}
		if strings.HasSuffix(c.Key, "Cert") {
			return name + " updated"
		}
		if c.Key == "protected" {
			return "Protected purchases changed"
		}
		if string(c.Before) == null {
			return fmt.Sprintf("%s set to %q", name, a)
		}
		return fmt.Sprintf("%s: %q → %q", name, b, a)
	case "rows":
		return c.Key
	}
	return c.Kind + " " + c.Key
}

// --- the assistant's write tools ---

func actionToolSpecs() []ollama.Tool {
	t := func(name, desc string, params map[string]any) ollama.Tool {
		return ollama.Tool{Type: "function", Function: ollama.ToolSpec{Name: name, Description: desc, Parameters: params}}
	}
	item := map[string]any{"type": "object", "properties": map[string]any{
		"id":       str("the Review item's record id"),
		"category": str("outflows: spend, sold, gift, donation, lost, self-transfer; inflows: income, gift-in, purchase, refund, self-transfer"),
		"usd":      num("USD amount when it matters: proceeds for sold, cost for purchase, value for income/spend (optional; the day's price is used when left out)"),
		"note":     str("a short note (optional)"),
	}, "required": []string{"id", "category"}}
	return []ollama.Tool{
		t("label_items", "Propose labels for items waiting in Review: list them in items, or describe them with match plus one category (for example every Lightning payment under $5 as a spend). The user sees a card and must press Apply (unless they turned confirmation off). Only call this when the user asks you to label something or agrees to your suggestion.", obj(map[string]any{
			"items":    map[string]any{"type": "array", "items": item, "description": "the items and their labels"},
			"match":    map[string]any{"type": "object", "description": "label every Review item matching: account, direction (in/out), network (lightning/onchain), min_usd, max_usd, from, to, text", "properties": map[string]any{"account": str(""), "direction": str(""), "network": str(""), "min_usd": num(""), "max_usd": num(""), "from": str(""), "to": str(""), "text": str("")}},
			"category": str("with match: the category for all of them"),
			"tiers":    map[string]any{"type": "array", "description": "instead of one category, a category per value band, checked in order: e.g. [{\"max_usd\":25,\"category\":\"spend\"},{\"category\":\"donation\"}] labels items up to $25 as spends and the rest as donations", "items": map[string]any{"type": "object", "properties": map[string]any{"max_usd": num("upper bound for this band (leave out for everything else)"), "category": str("")}}},
			"note":     str("with match: a note for all of them (optional)"),
		})),
		t("link_transfer", "Propose linking two Review items as one transfer between the user's own accounts (an outflow and the matching inflow). The user confirms with Apply.", obj(map[string]any{
			"out_id": str("record id of the outgoing side"),
			"in_id":  str("record id of the incoming side"),
		}, "out_id", "in_id")),
		t("protect_lot", "Propose protecting a purchase so its coins are sold last (or removing that protection). The user confirms with Apply.", obj(map[string]any{
			"id":      str("the purchase's record id, or a lot id from get_account"),
			"protect": map[string]any{"type": "boolean", "description": "true to protect, false to stop protecting"},
		}, "id")),
	}
}

var outCats = map[trace.Category]bool{trace.Spend: true, trace.Sold: true, trace.Gift: true, trace.Donation: true, trace.Lost: true, trace.SelfTransfer: true, trace.Untracked: true}
var inCats = map[trace.Category]bool{trace.IncomeIn: true, trace.GiftIn: true, trace.PurchaseIn: true, trace.RefundIn: true, trace.SelfTransfer: true, trace.Untracked: true}

// propose turns a write tool call into a pending action (or applies it
// when the user turned confirmation off). It returns what the model reads.
func (tb *toolbox) propose(name string, args map[string]any, lk *Lookup) (string, bool, error) {
	var cs []change
	var summary string
	findReview := func(id string) (trace.Item, error) {
		for _, it := range tb.rep.Unmatched {
			if sameRecord(it.Key, it.Txid, id) {
				return it, nil
			}
		}
		// A small model often passes the record as it read it
		// ("2025-09-11 Hot wallet out 0.04404630 BTC"): match on that.
		if it, ok := findByDescription(tb.rep, id); ok {
			return it, nil
		}
		for _, g := range tb.rep.Graph {
			if sameRecord(g.Key, g.Txid, id) {
				return trace.Item{}, fmt.Errorf("%s (%s) is already labeled, so it isn't waiting in Review. Look the items up again with analyze_records (source review) for their current ids, or call label_items with match (account, direction, network, min_usd, max_usd, from, to) and category", id, recordName(tb.rep, g.Key))
			}
		}
		return trace.Item{}, fmt.Errorf("%q isn't a Review item id. Ids look like [ex:12] or [tx:<txid>:0]; if you don't have them, call label_items again with match (account, direction, network, min_usd, max_usd, from, to) and category instead of items", id)
	}
	switch name {
	case "label_items":
		raw, _ := args["items"].([]any)
		// Wrong ids for items from the previous answer (a small model
		// copying them badly): label the items that answer listed instead,
		// when every item got the same category.
		if len(raw) > 0 && tb.refersBack && (len(tb.historyKeys) > 0 || tb.historyMatch != "") {
			bad, cat := false, ""
			for _, x := range raw {
				m, _ := x.(map[string]any)
				if _, err := findReview(argStr(m, "id")); err != nil {
					bad = true
				}
				c := argStr(m, "category")
				if cat != "" && c != cat {
					cat = "-"
				} else if cat == "" {
					cat = c
				}
			}
			if bad && cat != "" && cat != "-" {
				raw = tb.historyItems(cat)
				if raw == nil {
					var match map[string]any
					json.Unmarshal([]byte(tb.historyMatch), &match)
					args = map[string]any{"match": match, "category": cat}
				}
			}
		}
		// "Label these": the list the previous answer showed, exactly.
		if _, ok := args["match"].(map[string]any); ok && len(raw) == 0 && tb.refersBack {
			if items := tb.historyItems(argStr(args, "category")); items != nil {
				raw = items // exactly the rows that answer showed
			} else if tb.historyMatch != "" {
				var match map[string]any
				json.Unmarshal([]byte(tb.historyMatch), &match)
				args["match"] = match
			}
		}
		if m, ok := args["match"].(map[string]any); ok && len(raw) == 0 {
			items, err := tb.matchReview(m)
			if err != nil {
				return "", true, err
			}
			if len(items) == 0 {
				return "", true, errors.New("no Review items match that description")
			}
			cat := argStr(args, "category")
			if cat == "" && args["tiers"] == nil {
				return "", true, errors.New("give the category to apply (or tiers)")
			}
			for _, it := range items {
				raw = append(raw, map[string]any{"id": it.Key, "category": cat, "note": argStr(args, "note")})
			}
		}
		if len(raw) == 0 {
			return "", true, errors.New("give at least one item, or match and category")
		}
		if len(raw) > 500 {
			return "", true, fmt.Errorf("%d items match; narrow it to 500 or fewer", len(raw))
		}
		tiers, _ := args["tiers"].([]any)
		for _, x := range raw {
			m, _ := x.(map[string]any)
			it, err := findReview(argStr(m, "id"))
			if err != nil {
				return "", true, err
			}
			cat := trace.Category(strings.ToLower(argStr(m, "category")))
			if len(tiers) > 0 {
				cat = tierCategory(tiers, it.Value)
				if cat == "" {
					continue // outside every band
				}
			}
			ok := outCats[cat]
			if it.Dir == trace.In {
				ok = inCats[cat]
			}
			if !ok {
				return "", true, fmt.Errorf("%q doesn't fit %s, which is an %s", cat, recordName(tb.rep, it.Key), it.Dir)
			}
			l := trace.Label{Key: it.Key, Category: cat, Note: clip(argStr(m, "note"), 200)}
			if usd := argNum(m, "usd"); usd > 0 {
				l.FiatCents = int64(usd*100 + 0.5)
			}
			b, _ := json.Marshal(l)
			cs = append(cs, change{Kind: "label", Key: it.Key, After: b})
		}
		if len(cs) == 0 {
			return "", true, errors.New("none of those items fall in the bands you gave")
		}
		summary = fmt.Sprintf("Label %d Review item%s", len(cs), map[bool]string{true: "", false: "s"}[len(cs) == 1])
	case "link_transfer":
		out, err := findReview(argStr(args, "out_id"))
		if err != nil {
			return "", true, err
		}
		in, err := findReview(argStr(args, "in_id"))
		if err != nil {
			return "", true, err
		}
		if out.Dir != trace.Out || in.Dir != trace.In {
			if out.Dir == trace.In && in.Dir == trace.Out {
				out, in = in, out
			} else {
				return "", true, errors.New("a transfer links one outgoing item with one incoming item")
			}
		}
		if out.Account == in.Account {
			return "", true, errors.New("both items are in the same account")
		}
		b, _ := json.Marshal(trace.Label{Key: out.Key, Category: trace.SelfLink, Counterpart: in.Key})
		cs = append(cs, change{Kind: "label", Key: out.Key, After: b})
		summary = "Link a transfer from " + out.Account + " to " + in.Account
	case "protect_lot":
		id := strings.Trim(argStr(args, "id"), "[] ")
		key := ""
		for _, l := range tb.rep.OpenLots {
			if l.ID == id {
				key = l.OriginID
			}
		}
		for _, g := range tb.rep.Graph {
			if key == "" && sameRecord(g.Key, g.Txid, id) && (g.Kind == "buy" || g.Kind == "income" || g.Kind == "purchase" || g.Kind == "gift-in") {
				key = g.Key
			}
		}
		if key == "" {
			return "", true, fmt.Errorf("%q isn't a purchase or lot", id)
		}
		on := true
		if v, ok := args["protect"].(bool); ok {
			on = v
		}
		cs = append(cs, change{Kind: "protect", Key: key, After: json.RawMessage(strconv.FormatBool(on))})
		summary = map[bool]string{true: "Protect ", false: "Stop protecting "}[on] + recordName(tb.rep, key)
	default:
		return "", false, nil
	}
	b, _ := json.Marshal(cs)
	// The same proposal twice in one chat is one card.
	if recent, err := tb.s.st.Actions("assistant", 50); err == nil {
		for _, x := range recent {
			if x.Status == "pending" && x.ChatID == tb.chatID && sameState(x.Changes, b) {
				for _, id := range tb.actions {
					if id == x.ID {
						return fmt.Sprintf("Already proposed: %s (activity #%d). The card is in the chat; the user must press Apply.", x.Summary, x.ID), true, nil
					}
				}
				tb.actions = append(tb.actions, x.ID)
				lk.Summary, lk.Link = "Proposed: "+x.Summary, "#activity"
				return fmt.Sprintf("Proposed: %s (activity #%d). The user must press Apply on the card.", x.Summary, x.ID), true, nil
			}
		}
	}
	a := store.Action{Actor: "assistant", Kind: name, Summary: summary, Changes: b, Status: "pending", ChatID: tb.chatID, Undoable: true}
	// A new proposal replaces any earlier one still waiting in this chat,
	// so there's only ever one card to act on.
	var replaced []string
	if recent, err := tb.s.st.Actions("assistant", 50); err == nil {
		for _, x := range recent {
			if x.Status == "pending" && x.ChatID == tb.chatID {
				tb.s.st.UpdateAction(x.ID, "dismissed", nil)
				replaced = append(replaced, fmt.Sprintf("#%d", x.ID))
			}
		}
	}
	id, err := tb.s.st.AddAction(a)
	if err != nil {
		return "", true, err
	}
	a.ID = id
	auto := tb.s.st.Setting("aiAutoApply", "false") == "true"
	if auto {
		if err := tb.s.applyPending(&a); err != nil {
			return "", true, err
		}
	}
	tb.actions = append(tb.actions, id)
	lk.Summary, lk.Link = summary, "#activity"
	if auto {
		lk.Summary = "Applied: " + summary
		return fmt.Sprintf("Done: %s (activity #%d). The user can undo it from Activity. Tell them what changed.", summary, id), true, nil
	}
	lk.Summary = "Proposed: " + summary
	note := ""
	if len(replaced) > 0 {
		note = " It replaces the earlier proposal (" + strings.Join(replaced, ", ") + "), which was withdrawn."
	}
	return fmt.Sprintf("Proposed: %s (activity #%d).%s Nothing has changed yet: the user must press Apply on the card in the chat. Tell them what you proposed and why, and don't say it's done.", summary, id, note), true, nil
}

var settingNames = map[string]string{
	"fulcrumHost": "Fulcrum host", "fulcrumPort": "Fulcrum port", "fulcrumTLS": "Fulcrum TLS", "fulcrumCert": "Fulcrum certificate",
	"regtest": "Network (regtest)", "jurisdiction": "Jurisdiction", "method": "Cost basis method", "feeMode": "Network fees on own transfers",
	"rateShort": "Short-term rate %", "rateLong": "Long-term rate %", "priceSource": "Price source", "clnUrl": "Lightning node address",
	"clnCert": "Lightning certificate", "clnName": "Lightning node name", "aiUrl": "Assistant Ollama address", "aiModel": "Assistant model",
	"aiCtx": "Assistant memory", "aiThink": "Assistant thinks first", "aiAutoApply": "Apply assistant changes without asking",
}

// logAdded records rows that were just created (a wallet, an import), so
// undo removes them.
func (s *Server) logAdded(kind, summary string, sets []store.RowSet) {
	empty := make([]store.RowSet, len(sets))
	for i := range sets {
		empty[i] = sets[i]
		empty[i].Rows = []map[string]any{}
	}
	b, _ := json.Marshal(empty)
	s.finish("user", kind, summary, 0, []change{{Kind: "rows", Key: summary, Sets: sets, Before: b}})
}

// noted wraps a handler that changes things this log can't undo (scans,
// price updates, restores) and notes it when it succeeds.
func (s *Server) noted(kind, summary string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h(sw, r)
		if sw.code/100 == 2 {
			s.note(kind, summary)
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

// pastTense turns a proposal's wording into what was done.
func pastTense(s string) string {
	for _, p := range [][2]string{{"Stop protecting ", "Stopped protecting "}, {"Protect ", "Protected "}, {"Label ", "Labeled "}, {"Link ", "Linked "}} {
		if strings.HasPrefix(s, p[0]) {
			return p[1] + strings.TrimPrefix(s, p[0])
		}
	}
	return s
}

// bulkLabel labels many Review items at once, as one undoable action.
func (s *Server) bulkLabel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Keys     []string `json:"keys"`
		Category string   `json:"category"`
		Note     string   `json:"note"`
	}
	if err := decode(r, &in); err != nil || len(in.Keys) == 0 || len(in.Keys) > 500 {
		fail(w, 400, errors.New("send 1 to 500 keys and a category"))
		return
	}
	rep, err := s.buildReport()
	if err != nil {
		fail(w, 500, err)
		return
	}
	review := map[string]trace.Item{}
	for _, it := range rep.Unmatched {
		review[it.Key] = it
	}
	cat := trace.Category(in.Category)
	var cs []change
	for _, k := range in.Keys {
		it, ok := review[k]
		if !ok {
			continue // labeled since: skip
		}
		fits := outCats[cat]
		if it.Dir == trace.In {
			fits = inCats[cat]
		}
		if !fits {
			fail(w, 400, fmt.Errorf("%q doesn't fit %s, which is an %s", cat, recordName(rep, k), it.Dir))
			return
		}
		cs = append(cs, change{Kind: "label", Key: k})
	}
	if len(cs) == 0 {
		fail(w, 400, errors.New("none of these items are waiting in Review any more"))
		return
	}
	summary := fmt.Sprintf("Labeled %d Review item%s as %s", len(cs), map[bool]string{true: "", false: "s"}[len(cs) == 1], categoryNames[cat])
	id, err := s.record("user", "label", summary, 0, cs, func() error {
		for _, c := range cs {
			if err := s.st.SetLabel(trace.Label{Key: c.Key, Category: cat, Note: clip(in.Note, 200)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		fail(w, 500, err)
		return
	}
	a, err := s.st.Action(id)
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, s.viewActions([]store.Action{a})[0])
}

var (
	descDate = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
	descBTC  = regexp.MustCompile(`\b(\d+\.\d{8})\b`)
)

// findByDescription finds the one Review item a description names by its
// date, and its account and amount when given.
func findByDescription(rep *Report, desc string) (trace.Item, bool) {
	d := descDate.FindString(desc)
	if d == "" {
		return trace.Item{}, false
	}
	low := strings.ToLower(desc)
	var sats int64 = -1
	if m := descBTC.FindString(desc); m != "" {
		if f, err := strconv.ParseFloat(m, 64); err == nil {
			sats = int64(f*1e8 + 0.5)
		}
	}
	var hits []trace.Item
	for _, it := range rep.Unmatched {
		if dayOf(it.Time) != d {
			continue
		}
		if sats >= 0 && it.Sats != sats {
			continue
		}
		if strings.Contains(low, strings.ToLower(it.Account)) || sats >= 0 {
			hits = append(hits, it)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return trace.Item{}, false
}

// tierCategory picks the category of the first band an amount fits.
func tierCategory(tiers []any, cents int64) trace.Category {
	for _, x := range tiers {
		m, _ := x.(map[string]any)
		if _, ok := m["max_usd"]; !ok || float64(cents) <= argNum(m, "max_usd")*100 {
			return trace.Category(strings.ToLower(argStr(m, "category")))
		}
	}
	return ""
}

// historyItems are the rows the previous answer showed, as label items.
func (tb *toolbox) historyItems(cat string) []any {
	if len(tb.historyKeys) == 0 || cat == "" {
		return nil
	}
	var out []any
	for _, k := range tb.historyKeys {
		out = append(out, map[string]any{"id": k, "category": cat})
	}
	return out
}
