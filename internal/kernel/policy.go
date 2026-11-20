// Package kernel is Counterfoil's engine: the policy gate, the rules, the
// clocks, the package registry, the dispatcher and the step interpreter.
package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"counterfoil/internal/core"
)

// ReservedDef is one reserved decision. The brief's six are the floor:
// they load from policy/reserved.json with Floor=true and can never be
// removed or completed by an agent. HR may add entries, not remove these.
type ReservedDef struct {
	ID           string `json:"id"`
	Action       string `json:"action"` // the effect action it guards, "" = decision-only
	Title        string `json:"title"`
	ApproverRole string `json:"approver_role"` // role that may approve
	RouteOnly    bool   `json:"route_only"`    // POSH/grievance: route, never decide
	Floor        bool   `json:"floor"`
}

type reservedFile struct {
	Floor []ReservedDef `json:"floor"`
}

func LoadReserved(path string) ([]ReservedDef, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f reservedFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	for i := range f.Floor {
		f.Floor[i].Floor = true
	}
	return f.Floor, nil
}

// Reserved returns the reserved definition guarding an action, if any.
func (a *App) Reserved(action string) (ReservedDef, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.reserved {
		if r.Action == action && r.Action != "" {
			return r, true
		}
	}
	return ReservedDef{}, false
}

func (a *App) ReservedByID(id string) (ReservedDef, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.reserved {
		if r.ID == id {
			return r, true
		}
	}
	return ReservedDef{}, false
}

func (a *App) ReservedList() []ReservedDef {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ReservedDef, len(a.reserved))
	copy(out, a.reserved)
	return out
}

// AddReserved lets HR add a custom reserved action (never remove the floor).
func (a *App) AddReserved(by string, def ReservedDef) error {
	if def.ID == "" || def.Action == "" || def.ApproverRole == "" {
		return fmt.Errorf("id, action and approver role are required")
	}
	def.Floor = false
	a.mu.Lock()
	for _, r := range a.reserved {
		if r.ID == def.ID || (r.Action == def.Action && r.Action != "") {
			a.mu.Unlock()
			return fmt.Errorf("already reserved: %s", def.ID)
		}
	}
	a.mu.Unlock()
	return a.emit(core.Receipt{
		Action: "reserved.added", ActorType: "human", ActorID: by,
		Data: map[string]any{"id": def.ID, "action": def.Action, "title": def.Title,
			"approver_role": def.ApproverRole},
	})
}

// ---------------------------------------------------------------- kill switch

type KillSwitch struct {
	On    bool      `json:"on"`
	Scope string    `json:"scope"` // "all" or a case id
	By    string    `json:"by"`
	At    time.Time `json:"at"`
}

func (a *App) SetKillSwitch(by string, on bool, scope string) error {
	if scope == "" {
		scope = "all"
	}
	return a.emit(core.Receipt{
		Action: "killswitch.set", ActorType: "human", ActorID: by,
		Data: map[string]any{"on": on, "scope": scope},
	})
}

// Blocked reports whether the kill switch pauses work on a case. The pause
// blocks the NEXT action: nothing in flight is yanked, nothing new starts.
func (a *App) Blocked(caseID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.blockedLocked(caseID)
}

func (a *App) blockedLocked(caseID string) bool {
	if !a.kill.On {
		return false
	}
	return a.kill.Scope == "all" || a.kill.Scope == caseID
}

// GateEffect decides whether an effect may be dispatched now. It is the
// one checkpoint every outside action passes: the warrant.
//   - kill switch: blocks the next action;
//   - reserved actions: require a valid, unexpired, unused mandate whose
//     request hash equals the canonical hash of exactly this request;
//   - everything else: allowed, and still receipted.
//
// It returns the mandate to consume (if any) or a refusal error.
func (a *App) GateEffect(caseID, action string, args map[string]any) (*core.Mandate, error) {
	if a.Blocked(caseID) {
		return nil, fmt.Errorf("kill switch is on: the next action is blocked")
	}
	def, isReserved := a.Reserved(action)
	if !isReserved {
		return nil, nil
	}
	reqHash := core.MustHashCanon(Envelope(action, args))
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.mandates {
		if m.CaseID != caseID || m.ReservedID != def.ID {
			continue
		}
		if m.SingleUse && !m.UsedAt.IsZero() {
			continue
		}
		if time.Now().After(m.ExpiresAt) {
			continue
		}
		if m.RequestHash != reqHash {
			// A mandate exists but the outgoing bytes differ from what
			// the approver saw: refuse loudly, never "approximately".
			return nil, fmt.Errorf("REFUSED %s: request hash %s does not match mandate %s (approved %s)",
				def.ID, core.Short(reqHash), m.ID, core.Short(m.RequestHash))
		}
		cp := *m
		return &cp, nil
	}
	return nil, fmt.Errorf("REFUSED %s (%s): no valid mandate for this exact request", def.ID, def.Title)
}

// Envelope is the exact value a mandate's hash binds: the action and its
// typed arguments, nothing else. The approver's page shows these fields;
// the dispatcher recomputes this hash over the outgoing bytes.
func Envelope(action string, args map[string]any) map[string]any {
	return map[string]any{"action": action, "args": args}
}
