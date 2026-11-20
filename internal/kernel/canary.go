package kernel

import (
	"fmt"
	"time"

	"counterfoil/internal/core"
)

// Canary probes: every guard is re-tested with a request it must refuse or
// a false "done" it must not accept, and each result leaves a receipt. A
// guard that lets a canary through is news, not a hope. (Adapted from
// prior art credited in the README; run on demand from the admin page and
// after every package activation.)

type CanaryResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Held  bool   `json:"held"`
	Note  string `json:"note"`
}

func (a *App) RunCanaries(by string) []CanaryResult {
	const canaryCase = "CANARY"
	probes := []struct {
		id, title string
		run       func() (bool, string)
	}{
		{"C1", "A release with no mandate must be refused", func() (bool, string) {
			_, err := a.GateEffect(canaryCase, "payroll.release", map[string]any{"amount_inr": 1.0})
			return err != nil, errNote(err, "the gate accepted a release with no mandate")
		}},
		{"C2", "A tampered amount must not match its mandate", func() (bool, string) {
			req := map[string]any{"a": 1}
			h := core.MustHashCanon(req)
			tampered := map[string]any{"a": 2}
			return core.MustHashCanon(tampered) != h, "canonical hashes must differ when a field changes"
		}},
		{"C3", "An agent can never decide an approval", func() (bool, string) {
			err := a.DecideApproval("AP-NONE", "agent:dispatcher", "", true, nil, "")
			return err != nil, errNote(err, "an agent identity decided an approval")
		}},
		{"C4", "An expired mandate must be refused", func() (bool, string) {
			m := core.Mandate{ID: "MD-CANARY", CaseID: canaryCase, ReservedID: "RSV-6",
				RequestHash: core.MustHashCanon(Envelope("payroll.release", map[string]any{"x": 1})),
				IssuedAt:    time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
				SingleUse: true}
			a.mu.Lock()
			a.mandates[m.ID] = &m
			a.mu.Unlock()
			defer func() { a.mu.Lock(); delete(a.mandates, m.ID); a.mu.Unlock() }()
			_, err := a.GateEffect(canaryCase, "payroll.release", map[string]any{"x": 1})
			return err != nil, errNote(err, "an expired mandate was accepted")
		}},
		{"C5", "A duplicate intent must not send twice", func() (bool, string) {
			key := core.IntentKey(canaryCase, "probe", "canary.create", map[string]any{"n": 1})
			_, began1, _ := a.ledger.Begin(core.Intent{Key: key, CaseID: canaryCase, StepID: "probe",
				Action: "canary.create", Target: "canarysys"})
			_, began2, _ := a.ledger.Begin(core.Intent{Key: key, CaseID: canaryCase, StepID: "probe",
				Action: "canary.create", Target: "canarysys"})
			return began2 == false, fmt.Sprintf("first begin %v, second begin %v (must be false)", began1, began2)
		}},
		{"C6", "A lying success reply must not become Done", func() (bool, string) {
			// canarysys always replies 201 and stores nothing.
			def := StepDef{ID: "probe", Effect: &EffectDef{
				Action: "canary.create", Target: "canarysys", Path: "/canarysys/items",
				ReadBack:         ReadBack{Strategy: "attributed", Collection: "canarysys/items"},
				RequiredEvidence: core.EvAttributed}}
			key := core.IntentKey(canaryCase, "probe-lie", "canary.create",
				map[string]any{"probe": by + fdate(time.Now())})
			in, _, _ := a.ledger.Begin(core.Intent{Key: key, CaseID: canaryCase, StepID: "probe-lie",
				Action: "canary.create", Target: "canarysys",
				Args: map[string]any{"probe": by + fdate(time.Now())}})
			out := a.sendAndReadBack(in, def, true)
			return out.verdict != "done", "verdict " + out.verdict + " (a lying 201 must never be Done)"
		}},
		{"C7", "The kill switch must block the next dispatch", func() (bool, string) {
			a.mu.Lock()
			saved := a.kill
			a.kill = KillSwitch{On: true, Scope: "all", By: "canary", At: time.Now()}
			a.mu.Unlock()
			_, err := a.GateEffect(canaryCase, "itsm.ticket.create", map[string]any{"x": 1})
			a.mu.Lock()
			a.kill = saved
			a.mu.Unlock()
			return err != nil, errNote(err, "a dispatch passed the gate with the switch on")
		}},
	}
	var out []CanaryResult
	for _, p := range probes {
		held, note := p.run()
		out = append(out, CanaryResult{ID: p.id, Title: p.title, Held: held, Note: note})
		_ = a.emit(core.Receipt{
			Action: "canary.run", ActorType: "human", ActorID: by,
			Data: map[string]any{"id": p.id, "title": p.title, "held": held, "note": note},
		})
	}
	return out
}

func errNote(err error, failMsg string) string {
	if err != nil {
		return "refused: " + err.Error()
	}
	return "NOT HELD: " + failMsg
}
