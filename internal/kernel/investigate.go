// Read-only investigation queries for the in-doubt assistant. These are
// the backend of the model's tools on that surface: each one derives
// what to read from the item itself — the model never supplies a URL, a
// path or a record id, so it can only look at what the item is about.
package kernel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"counterfoil/internal/core"
)

// StepDefFor exposes the step declaration lookup to the surfaces.
func (a *App) StepDefFor(caseID, stepID string) (StepDef, bool) {
	return a.stepDef(caseID, stepID)
}

// InDoubtItemByID finds one item, open or resolved.
func (a *App) InDoubtItemByID(id string) (*core.InDoubtItem, bool) {
	for _, it := range a.InDoubt(false) {
		if it.ID == id {
			return it, true
		}
	}
	return nil, false
}

// InDoubtDetail assembles what the platform itself knows about an item:
// the step's declaration, the intent ledger's state, and whether the
// step is reserved (and therefore can only ever be re-dispatched under
// its mandate, never hand-closed).
func (a *App) InDoubtDetail(itemID string) (map[string]any, error) {
	it, ok := a.InDoubtItemByID(itemID)
	if !ok {
		return nil, fmt.Errorf("no in-doubt item %s", itemID)
	}
	out := map[string]any{
		"item_id": it.ID, "case_id": it.CaseID, "step_id": it.StepID,
		"reason": it.Reason, "owner_role": it.OwnerRole, "open": it.Open,
	}
	var ev []map[string]any
	for _, f := range it.Evidence {
		ev = append(ev, map[string]any{"label": f.Label, "value": f.Value, "source": f.Source})
	}
	if ev != nil {
		out["verifier_evidence"] = ev
	}
	if def, ok := a.stepDef(it.CaseID, it.StepID); ok {
		stepInfo := map[string]any{"title": def.Title, "kind": string(def.Kind)}
		if def.Reserved != "" {
			stepInfo["reserved"] = def.Reserved
			stepInfo["note"] = "reserved step: it can only be re-dispatched under its mandate — never closed or cancelled by hand"
		}
		if def.Effect != nil {
			stepInfo["effect_action"] = def.Effect.Action
			stepInfo["target_system"] = def.Effect.Target
			stepInfo["read_back_strategy"] = def.Effect.ReadBack.Strategy
			stepInfo["retry_safe"] = def.Effect.RetrySafe
			stepInfo["reversibility"] = def.Effect.Reversibility
		}
		out["step"] = stepInfo
	}
	if it.IntentKey != "" {
		if in, ok := a.ledger.Get(it.IntentKey); ok {
			out["intent"] = map[string]any{
				"state": string(in.State), "attempts": in.Attempts,
				"action": in.Action, "args": in.Args,
				"external_ref": in.ExternalRef, "evidence": in.Evidence,
				"note": in.Note, "mandate_id": in.MandateID,
				"client_ref": core.Short(in.Key),
			}
		}
	}
	return out, nil
}

// ReadTargetForItem performs a fresh read-back against the target system
// for the item's own intent, with the same production logic the
// dispatcher uses, and returns the verdict plus the raw records found by
// our client reference. Server-derived end to end: nothing the model
// says changes where this reads.
func (a *App) ReadTargetForItem(itemID string) (map[string]any, error) {
	it, ok := a.InDoubtItemByID(itemID)
	if !ok {
		return nil, fmt.Errorf("no in-doubt item %s", itemID)
	}
	if it.IntentKey == "" {
		return nil, fmt.Errorf("item %s has no external intent to read back", itemID)
	}
	in, ok := a.ledger.Get(it.IntentKey)
	if !ok {
		return nil, fmt.Errorf("intent for item %s is not in the ledger", itemID)
	}
	def, ok := a.stepDef(it.CaseID, it.StepID)
	if !ok || def.Effect == nil {
		return nil, fmt.Errorf("step %s has no external effect declared", it.StepID)
	}
	verdict, evidence, extRef, note := a.readBack(in, def)
	out := map[string]any{
		"verdict": verdict, "evidence_level": evidence, "note": note,
		"target_system": def.Effect.Target, "client_ref": core.Short(in.Key),
	}
	if extRef != "" {
		out["record_id"] = extRef
	}
	if recs, err := a.mockList(def.Effect.ReadBack.Collection, core.Short(in.Key)); err == nil {
		out["records_by_our_reference"] = recs
	}
	return out, nil
}

// mockList fetches the target collection filtered by our reference,
// trimmed to a handful of records.
func (a *App) mockList(collection, clientRef string) ([]map[string]any, error) {
	client := &http.Client{Timeout: a.cfg.DispatchTimeout}
	resp, err := client.Get(fmt.Sprintf("%s/%s?client_ref=%s", a.cfg.MocksURL, collection, clientRef))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("target status %d", resp.StatusCode)
	}
	var m []map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, err
	}
	if len(m) > 5 {
		m = m[:5]
	}
	return m, nil
}

// ReceiptSummaries condenses a case's receipts for the model: sequence,
// action, actor and note-like fields only — enough to reason over the
// order of events without the full payloads.
func (a *App) ReceiptSummaries(caseID string, limit int) []map[string]any {
	rs := a.Receipts(0, caseID)
	sort.Slice(rs, func(i, j int) bool { return rs[i].Seq < rs[j].Seq })
	if limit > 0 && len(rs) > limit {
		rs = rs[len(rs)-limit:]
	}
	var out []map[string]any
	for _, r := range rs {
		row := map[string]any{"seq": r.Seq, "action": r.Action, "actor": r.ActorType}
		if r.Evidence != "" {
			row["evidence"] = r.Evidence
		}
		for _, k := range []string{"note", "reason", "status", "verdict"} {
			if v, ok := r.Data[k].(string); ok && v != "" {
				row[k] = v
			}
		}
		out = append(out, row)
	}
	return out
}
