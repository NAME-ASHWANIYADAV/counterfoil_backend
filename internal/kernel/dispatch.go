package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"counterfoil/internal/core"
)

// Dispatch is the warrant path for one external effect:
//
//	gate (policy + mandate + kill switch)
//	-> intent ledger PENDING, synced before any bytes leave
//	-> send once, with Idempotency-Key and X-Client-Ref
//	-> read back the target's own record
//	-> Done (with its evidence level) or In doubt — never a guess.
//
// Verdicts follow the S1 spike and the credited prior art (Anchor):
// applied+attributed, applied+observed, not-applied, unknown. "The reply
// said OK" is never enough on its own.

type dispatchOutcome struct {
	intent   core.Intent
	verdict  string // done | in_doubt | refused
	evidence string
	note     string
}

// runEffect executes one effect step end to end. It must be called
// without holding a.mu. It emits all receipts itself.
func (a *App) runEffect(c *core.Case, def StepDef) {
	e := def.Effect
	args := a.effectArgs(c, def)

	// Demo tamper switch: change the bytes AFTER approval, BEFORE the
	// gate — on the next RESERVED dispatch only, as the control promises.
	// The gate must catch it; that is the point of the drill.
	if a.TamperNext() {
		if _, isReserved := a.Reserved(def.Effect.Action); isReserved {
			if amt, ok := args["amount_inr"].(float64); ok {
				args["amount_inr"] = amt + 1
				a.SetTamperNext(false)
				_ = a.emit(core.Receipt{CaseID: c.ID, StepID: def.ID, Action: "demo.tamper",
					ActorType: "system", Data: map[string]any{"note": "demo control changed the amount by ₹1 after approval"}})
			}
		}
	}
	req := Envelope(e.Action, args)

	mandate, gateErr := a.GateEffect(c.ID, e.Action, args)
	if gateErr != nil {
		_ = a.emit(core.Receipt{
			CaseID: c.ID, StepID: def.ID, Action: "effect.refused", ActorType: "system",
			Data: map[string]any{"action": e.Action, "reason": gateErr.Error(),
				"request_hash": core.MustHashCanon(req)},
		})
		a.openInDoubt(c.ID, def.ID, "", "hrops",
			fmt.Sprintf("dispatch refused: %s", gateErr.Error()), nil)
		return
	}
	mandateID := ""
	if mandate != nil {
		mandateID = mandate.ID
		_ = a.emit(core.Receipt{CaseID: c.ID, StepID: def.ID, Action: "mandate.used",
			ActorType: "system", Data: map[string]any{"mandate_id": mandate.ID,
				"request_hash": mandate.RequestHash}})
	}

	// Internal effects: cfoil itself is the system of record.
	if e.Target == "internal" {
		a.commitInternal(c, def, args, mandateID)
		return
	}

	key := core.IntentKey(c.ID, def.ID, e.Action, args)
	in, began, err := a.ledger.Begin(core.Intent{
		Key: key, CaseID: c.ID, StepID: def.ID, Action: e.Action, Target: e.Target,
		Args: args, MandateID: mandateID,
	})
	if err != nil {
		a.openInDoubt(c.ID, def.ID, key, "hrops", "ledger write failed: "+err.Error(), nil)
		return
	}
	if !began {
		// The intent already exists: never send again blindly.
		switch in.State {
		case core.IntentCommitted:
			return
		case core.IntentPending:
			a.reconcileIntent(in, def)
			return
		default:
			return
		}
	}
	_ = a.emit(core.Receipt{
		CaseID: c.ID, StepID: def.ID, Action: "effect.dispatched", ActorType: "agent",
		ActorID: "dispatcher",
		Data: map[string]any{"action": e.Action, "target": e.Target,
			"intent_key": core.Short(key), "request_hash": core.MustHashCanon(req),
			"mandate_id": mandateID},
	})
	out := a.sendAndReadBack(in, def, true)
	a.settleOutcome(c.ID, def, out)
}

// effectArgs builds the typed argument set for an effect from the case and
// its facts. Dates, money and identifiers come from rules and mandates —
// never from model text (INV: words cannot decide).
func (a *App) effectArgs(c *core.Case, def StepDef) map[string]any {
	emp, _ := a.Employee(c.EmployeeID)
	args := map[string]any{"employee_id": emp.ID}
	switch def.Effect.Action {
	case "separation.accept":
		args["last_working_day"] = c.BoundLWD
		args["notice_buyout"] = c.Buyout
		args["buyout_amount_inr"] = factFloat(c, "buyout_amount")
	case "itsm.ticket.create":
		if c.Package == "onboarding" {
			kind := "laptop_provision"
			if def.ID == "card_issue" {
				kind = "card_issue"
			}
			args["type"] = kind
			args["name"] = emp.Name
			args["due"] = factStr(c, "joining_date")
			break
		}
		asset := ""
		if len(emp.Assets) > 0 {
			asset = emp.Assets[0]
		}
		args["type"] = "asset_return"
		args["asset"] = asset
		args["due"] = c.BoundLWD
	case "directory.disable":
		args["email"] = emp.Email
	case "cards.revoke":
		args["card_id"] = emp.CardID
	case "payroll.stage":
		if c.Package == "onboarding" {
			args["payroll_id"] = emp.PayrollID
			args["type"] = "setup"
			args["monthly_gross_inr"] = emp.MonthlyGross
			args["start"] = factStr(c, "joining_date")
			break
		}
		args["payroll_id"] = emp.PayrollID
		args["amount_inr"] = factFloat(c, "ff_total")
		args["period"] = factStr(c, "ff_period")
		args["status"] = "held"
	case "payroll.release":
		args["payroll_id"] = emp.PayrollID
		args["amount_inr"] = factFloat(c, "ff_total")
		args["period"] = factStr(c, "ff_period")
		args["type"] = "full_and_final"
	case "canary.create":
		args["probe"] = factStr(c, "canary_probe")
	}
	return args
}

func factStr(c *core.Case, k string) string {
	if c.Facts == nil {
		return ""
	}
	s, _ := c.Facts[k].(string)
	return s
}

func factFloat(c *core.Case, k string) float64 {
	if c.Facts == nil {
		return 0
	}
	f, _ := c.Facts[k].(float64)
	return f
}

// commitInternal applies an internal effect (our own record) and emits its
// action receipt so clocks can close on it.
func (a *App) commitInternal(c *core.Case, def StepDef, args map[string]any, mandateID string) {
	action := map[string]string{
		"separation.accept": "separation.accepted",
		"document.issue":    "document.issued",
		"case.close":        "case.closed",
	}[def.Effect.Action]
	if action == "" {
		action = def.Effect.Action
	}
	_ = a.emit(core.Receipt{
		CaseID: c.ID, StepID: def.ID, Action: action, ActorType: "agent", ActorID: "kernel",
		Evidence: "internal",
		Data: map[string]any{"args_hash": core.MustHashCanon(args), "mandate_id": mandateID,
			"args": publicArgs(args)},
	})
	actor, human := a.stepCompleter(def, mandateID)
	note := ""
	if mandateID != "" {
		note = "completed under mandate " + mandateID
	}
	a.markStepDone(c.ID, def, actor, human, "internal", note)
}

// stepCompleter decides who a finished step is credited to: the approver
// for an approval step (their mandate released it), the agent otherwise.
func (a *App) stepCompleter(def StepDef, mandateID string) (string, bool) {
	if def.Kind == core.StepApproval && mandateID != "" {
		if m, ok := a.mandateByID(mandateID); ok {
			return m.ApproverID, true
		}
	}
	return "agent:dispatcher", false
}

func (a *App) mandateByID(id string) (core.Mandate, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.mandates[id]
	if !ok {
		return core.Mandate{}, false
	}
	return *m, true
}

func publicArgs(args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	return out
}

// sendAndReadBack performs the one send and the read-back. firstSend=false
// on reconciliation paths (restart): then nothing is sent unless the
// read-back proves nothing stands AND the effect is retry-safe.
func (a *App) sendAndReadBack(in core.Intent, def StepDef, firstSend bool) dispatchOutcome {
	e := def.Effect
	client := &http.Client{Timeout: a.cfg.DispatchTimeout}
	sendErr := ""
	var replyID string
	var replyStatus int

	if firstSend {
		body, _ := json.Marshal(in.Args)
		reqURL := a.cfg.MocksURL + e.Path
		httpReq, _ := http.NewRequest("POST", reqURL, bytes.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Idempotency-Key", in.Key)
		httpReq.Header.Set("X-Client-Ref", core.Short(in.Key))
		resp, err := client.Do(httpReq)
		if err != nil {
			sendErr = err.Error()
		} else {
			replyStatus = resp.StatusCode
			var rb map[string]any
			_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rb)
			resp.Body.Close()
			replyID = str(rb, "id")
		}
	}

	// Read-back: the target's own record, by our reference.
	verdict, evidence, extRef, note := a.readBack(in, def)

	switch {
	case verdict == "applied":
		st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) {
			x.State = core.IntentCommitted
			x.ExternalRef = extRef
			x.Evidence = evidence
			x.Note = note
		})
		return dispatchOutcome{intent: st, verdict: "done", evidence: evidence, note: note}

	case verdict == "not_applied" && replyStatus >= 200 && replyStatus < 300:
		// The reply claimed success and the record is not there: a false
		// OK. Never retry a contradiction; a person decides.
		st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) {
			x.State = core.IntentInDoubt
			x.Note = fmt.Sprintf("target replied %d (id %s) but its own records hold nothing", replyStatus, replyID)
		})
		return dispatchOutcome{intent: st, verdict: "in_doubt", evidence: core.EvReplyOnly,
			note: st.Note}

	case verdict == "not_applied" && sendErr != "" && e.RetrySafe && in.Attempts < 2:
		// Transport failed and the target shows nothing: one careful
		// retry, only where the target honours our key or the action
		// converges.
		st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) { x.Attempts++ })
		_ = a.emit(core.Receipt{CaseID: in.CaseID, StepID: in.StepID, Action: "effect.retried",
			ActorType: "agent", ActorID: "dispatcher",
			Data: map[string]any{"intent_key": core.Short(in.Key), "attempt": st.Attempts,
				"why": "transport error and read-back found nothing; target enforces our request ID"}})
		return a.sendAndReadBack(st, def, true)

	case verdict == "not_applied":
		st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) {
			x.State = core.IntentInDoubt
			x.Note = "nothing stands in the target and no safe retry is available: " + firstNonEmpty(sendErr, note)
		})
		return dispatchOutcome{intent: st, verdict: "in_doubt", evidence: "", note: st.Note}

	default: // unknown
		st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) {
			x.Note = "outcome unknown: " + firstNonEmpty(note, sendErr)
		})
		return dispatchOutcome{intent: st, verdict: "pending", evidence: "", note: st.Note}
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// readBack asks the target what stands, by our client reference, and
// grades the evidence. applied/attributed needs the record to carry our
// reference; a matching record without it is only "observed".
func (a *App) readBack(in core.Intent, def StepDef) (verdict, evidence, extRef, note string) {
	e := def.Effect
	if e.ReadBack.Strategy == "none" {
		return "applied", core.EvReplyOnly, "", "no read-back declared: " + e.ReadBack.Justification
	}
	client := &http.Client{Timeout: a.cfg.DispatchTimeout}
	ref := core.Short(in.Key)
	u := fmt.Sprintf("%s/%s?client_ref=%s", a.cfg.MocksURL, e.ReadBack.Collection, url.QueryEscape(ref))
	var lastErr string
	for try := 0; try < 3; try++ {
		if try > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		resp, err := client.Get(u)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		var recs []map[string]any
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&recs)
		resp.Body.Close()
		if err != nil {
			lastErr = "read-back decode: " + err.Error()
			continue
		}
		if len(recs) == 1 && fieldsMatch(recs[0], in.Args) {
			return "applied", core.EvAttributed, str(recs[0], "id"),
				"the target's record carries our reference and the bound fields match"
		}
		if len(recs) > 1 {
			return "unknown", "", "", fmt.Sprintf("read-back by our reference found %d records", len(recs))
		}
		// Nothing by our reference: fall back to an observed lookup where
		// the target supports it (convergent actions like disable/revoke).
		if e.ReadBack.Strategy == "attributed" || e.ReadBack.Strategy == "observed" {
			if obs, id, ok := a.observedLookup(in, def); ok {
				return "applied", obs, id, "the record exists but cannot be tied to our request (observed, not attributed)"
			}
		}
		return "not_applied", "", "", "the target's own records hold nothing for our reference"
	}
	return "unknown", "", "", lastErr
}

// observedLookup checks state-style targets (directory, cards) where the
// end state can exist without our reference on it.
func (a *App) observedLookup(in core.Intent, def StepDef) (string, string, bool) {
	e := def.Effect
	var probe string
	switch e.Action {
	case "directory.disable":
		probe = fmt.Sprintf("%s/directory/users/%s", a.cfg.MocksURL, url.PathEscape(str(in.Args, "email")))
	case "cards.revoke":
		probe = fmt.Sprintf("%s/cards/cards/%s", a.cfg.MocksURL, url.PathEscape(str(in.Args, "card_id")))
	default:
		return "", "", false
	}
	client := &http.Client{Timeout: a.cfg.DispatchTimeout}
	resp, err := client.Get(probe)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", false
	}
	var rec map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rec); err != nil {
		return "", "", false
	}
	status := str(rec, "status")
	if (e.Action == "directory.disable" && status == "disabled") ||
		(e.Action == "cards.revoke" && status == "revoked") {
		if str(rec, "last_action_ref") == core.Short(in.Key) {
			return core.EvAttributed, str(rec, "id"), true
		}
		return core.EvObserved, str(rec, "id"), true
	}
	return "", "", false
}

func fieldsMatch(rec map[string]any, args map[string]any) bool {
	for k, want := range args {
		if k == "status" {
			continue
		}
		got, ok := rec[k]
		if !ok {
			continue // the target may not echo every field
		}
		wb, _ := json.Marshal(want)
		gb, _ := json.Marshal(got)
		if string(wb) != string(gb) {
			return false
		}
	}
	return true
}

// settleOutcome turns a dispatch outcome into receipts and step state.
func (a *App) settleOutcome(caseID string, def StepDef, out dispatchOutcome) {
	e := def.Effect
	switch out.verdict {
	case "done":
		required := e.RequiredEvidence
		_ = a.emit(core.Receipt{
			CaseID: caseID, StepID: def.ID, Action: "effect.readback", ActorType: "agent",
			ActorID: "verifier", Evidence: out.evidence,
			Data: map[string]any{"action": e.Action, "intent_key": core.Short(out.intent.Key),
				"external_ref": out.intent.ExternalRef, "note": out.note},
		})
		if !evidenceAtLeast(out.evidence, required) {
			a.openInDoubt(caseID, def.ID, out.intent.Key, ownerRoleFor(e),
				fmt.Sprintf("evidence %s is below the level this step requires (%s)", out.evidence, required),
				[]core.Fact{{Label: "External ref", Value: out.intent.ExternalRef, Source: e.Target}})
			return
		}
		// Action-specific receipt for clocks (payment.released etc.).
		if e.Action == "payroll.release" {
			_ = a.emit(core.Receipt{CaseID: caseID, StepID: def.ID, Action: "payment.released",
				ActorType: "agent", ActorID: "dispatcher", Evidence: out.evidence,
				Data: map[string]any{"payment_ref": out.intent.ExternalRef,
					"amount_inr": out.intent.Args["amount_inr"],
					"mandate_id": out.intent.MandateID}})
		}
		actor, human := a.stepCompleter(def, out.intent.MandateID)
		a.markStepDone(caseID, def, actor, human, out.evidence, out.note)
	case "in_doubt":
		a.openInDoubt(caseID, def.ID, out.intent.Key, ownerRoleFor(e), out.note, nil)
	case "refused":
		a.openInDoubt(caseID, def.ID, out.intent.Key, "hrops", out.note, nil)
	case "pending":
		// Left PENDING; the tick reconciler keeps asking until the settle
		// window passes, then opens an in-doubt item.
	}
}

func ownerRoleFor(e *EffectDef) string {
	if e.Target == "payroll" {
		return "payroll"
	}
	return "hrops"
}

// reconcileIntent re-establishes a PENDING intent after a crash or an
// unknown outcome. It only observes; it never re-executes on its own
// unless the target shows nothing AND the effect is retry-safe.
func (a *App) reconcileIntent(in core.Intent, def StepDef) {
	verdict, evidence, extRef, note := a.readBack(in, def)
	switch verdict {
	case "applied":
		st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) {
			x.State = core.IntentCommitted
			x.ExternalRef = extRef
			x.Evidence = evidence
			x.Note = "reconciled after restart: " + note
		})
		_ = a.emit(core.Receipt{CaseID: in.CaseID, StepID: in.StepID, Action: "effect.reconciled",
			ActorType: "agent", ActorID: "reconciler", Evidence: evidence,
			Data: map[string]any{"intent_key": core.Short(in.Key), "external_ref": extRef,
				"note": "found the original effect; nothing was sent again"}})
		a.settleOutcome(in.CaseID, def, dispatchOutcome{intent: st, verdict: "done", evidence: evidence, note: note})
	case "not_applied":
		if def.Effect.RetrySafe && in.Attempts < 2 {
			st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) { x.Attempts++ })
			_ = a.emit(core.Receipt{CaseID: in.CaseID, StepID: in.StepID, Action: "effect.retried",
				ActorType: "agent", ActorID: "reconciler",
				Data: map[string]any{"intent_key": core.Short(in.Key), "attempt": st.Attempts,
					"why": "restart reconciliation: the target holds nothing and enforces our request ID"}})
			out := a.sendAndReadBack(st, def, true)
			a.settleOutcome(in.CaseID, def, out)
		} else {
			st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) {
				x.State = core.IntentInDoubt
				x.Note = "restart reconciliation: outcome cannot be established safely"
			})
			a.settleOutcome(in.CaseID, def, dispatchOutcome{intent: st, verdict: "in_doubt", note: st.Note})
		}
	default:
		if time.Since(in.Updated) > a.cfg.SettleWait*4 {
			st, _ := a.ledger.Transition(in.Key, func(x *core.Intent) {
				x.State = core.IntentInDoubt
				x.Note = "outcome unknown past the settle window: " + note
			})
			a.settleOutcome(in.CaseID, def, dispatchOutcome{intent: st, verdict: "in_doubt", note: st.Note})
		}
	}
}
