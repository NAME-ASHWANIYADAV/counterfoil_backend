package kernel

import (
	"fmt"
	"time"

	"counterfoil/internal/core"
)

// Clocks: an exit is a set of deadlines. A clock closes only on a receipt
// at the evidence level the clock declares — a reply cannot close it, an
// agent's message cannot, a timeout cannot, and the kill switch does not
// pause it. (Pausing agents must never silently extend a statutory date.)

// startClocks opens the obligations for a case at acceptance. Called with
// the bound LWD from the approver's mandate.
func (a *App) startClocks(caseID string, emp core.Employee, boundLWD string, ffEligibleGratuity bool) error {
	lwd, err := parseDate(boundLWD)
	if err != nil {
		return err
	}
	rp := a.rules

	mk := func(ob core.Obligation) error {
		ob.CaseID = caseID
		ob.Status = core.ClockOpen
		return a.emit(core.Receipt{
			CaseID: caseID, Action: "obligation.started", ActorType: "system",
			Data: map[string]any{"obligation": ob},
		})
	}

	// Final wages: N working days from the last working day.
	due, skipped := rp.AddWorkingDays(lwd, rp.FinalWages.WorkingDays)
	if err := mk(core.Obligation{
		ID: "OB-" + caseID + "-wages", RuleID: "final_wages",
		Title: fmt.Sprintf("Final wages within %d working days of the last working day", rp.FinalWages.WorkingDays),
		Basis: rp.FinalWages.Cite, NeedsLegalConfirmation: rp.FinalWages.NeedsLegalConfirmation,
		StartEvent: "separation.accepted", StartDate: boundLWD,
		DueDate: fdate(due), SkippedDays: skipped,
		EvidenceRequired: core.EvAttributed, ClosesOnAction: "payment.released",
	}); err != nil {
		return err
	}

	// Relieving letter: policy days, calendar.
	rl, _ := parseDate(boundLWD)
	rlDue := rl.Add(time.Duration(rp.RelievingLetter.Days) * day)
	if err := mk(core.Obligation{
		ID: "OB-" + caseID + "-letter", RuleID: "relieving_letter",
		Title: fmt.Sprintf("Relieving letter within %d days of the last working day", rp.RelievingLetter.Days),
		Basis: rp.RelievingLetter.Cite,
		StartEvent: "separation.accepted", StartDate: boundLWD,
		DueDate: fdate(rlDue),
		EvidenceRequired: "internal", ClosesOnAction: "document.issued",
	}); err != nil {
		return err
	}

	// Gratuity: only when eligible.
	if ffEligibleGratuity {
		gd, _ := parseDate(boundLWD)
		gDue := gd.Add(time.Duration(rp.Gratuity.DueDays) * day)
		if err := mk(core.Obligation{
			ID: "OB-" + caseID + "-gratuity", RuleID: "gratuity",
			Title: fmt.Sprintf("Gratuity within %d days", rp.Gratuity.DueDays),
			Basis: rp.Gratuity.Cite, NeedsLegalConfirmation: rp.Gratuity.NeedsLegalConfirmation,
			StartEvent: "separation.accepted", StartDate: boundLWD,
			DueDate: fdate(gDue),
			EvidenceRequired: core.EvAttributed, ClosesOnAction: "payment.released",
		}); err != nil {
			return err
		}
	}
	return nil
}

// tryCloseClocks is called after a receipt that may satisfy a clock. The
// closing rule: matching action, matching case, evidence at or above the
// declared level.
func (a *App) tryCloseClocks(r core.Receipt) {
	if r.CaseID == "" {
		return
	}
	a.mu.Lock()
	var due []core.Obligation
	for _, ob := range a.obligations {
		if ob.CaseID == r.CaseID && (ob.Status == core.ClockOpen || ob.Status == core.ClockAtRisk || ob.Status == core.ClockBreached) &&
			ob.ClosesOnAction == r.Action && evidenceAtLeast(r.Evidence, ob.EvidenceRequired) {
			due = append(due, *ob)
		}
	}
	a.mu.Unlock()
	for _, ob := range due {
		_ = a.emit(core.Receipt{
			CaseID: r.CaseID, Action: "obligation.closed", ActorType: "system",
			Evidence: r.Evidence,
			Data: map[string]any{"obligation_id": ob.ID, "closed_by_receipt": r.Seq,
				"on_time": fdate(time.Now()) <= ob.DueDate},
		})
	}
}

func evidenceAtLeast(got, need string) bool {
	rank := map[string]int{core.EvReplyOnly: 1, core.EvObserved: 2, core.EvAttributed: 3, "internal": 3}
	g, n := rank[got], rank[need]
	if n == 0 {
		return true
	}
	return g >= n
}

// refreshClocks flags clocks at risk (due within one working day) and
// breached (past due). It runs on the engine tick.
func (a *App) refreshClocks() {
	today := fdate(time.Now())
	a.mu.Lock()
	type change struct {
		id   string
		next core.ClockStatus
	}
	var changes []change
	for _, ob := range a.obligations {
		if ob.Status == core.ClockClosed {
			continue
		}
		next := ob.Status
		if today > ob.DueDate {
			next = core.ClockBreached
		} else {
			d, err := parseDate(ob.DueDate)
			warn := ob.Status
			if err == nil {
				if t, _ := parseDate(today); !t.Add(2 * day).Before(d) {
					warn = core.ClockAtRisk
				} else {
					warn = core.ClockOpen
				}
			}
			next = warn
		}
		if next != ob.Status {
			changes = append(changes, change{ob.ID, next})
		}
	}
	a.mu.Unlock()
	for _, c := range changes {
		_ = a.emit(core.Receipt{
			Action: "obligation.status", ActorType: "system",
			Data: map[string]any{"obligation_id": c.id, "status": string(c.next)},
		})
	}
}
