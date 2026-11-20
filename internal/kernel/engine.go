package kernel

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"counterfoil/internal/core"
)

// The engine interprets a package's steps. Agents and rules run on their
// own; approvals and human inputs wait for their named people; effects go
// down the warrant path. The kill switch stops the next action, and a
// restart resumes from the receipts and the intent ledger.

// Start begins the background tick.
func (a *App) Start() {
	go func() {
		// Reconcile anything the last run left PENDING before new work.
		a.ReconcilePending()
		t := time.NewTicker(2 * time.Second)
		for range t.C {
			a.Tick()
		}
	}()
}

// Tick advances every open case and refreshes the clocks.
func (a *App) Tick() {
	for _, c := range a.Cases() {
		if c.Status != core.CaseOpen {
			continue
		}
		a.Evaluate(c.ID)
		// The deadline monitor keeps watching until every clock closes.
		for _, def := range a.StepDefs(c) {
			if def.Impl == "deadline_monitor" {
				if st := c.Steps[def.ID]; st != nil && st.Status == core.StepRunning {
					a.runImpl(c.ID, def)
				}
			}
		}
	}
	a.refreshClocks()
}

// ReconcilePending re-establishes PENDING intents (crash recovery).
func (a *App) ReconcilePending() {
	for _, in := range a.ledger.Pending() {
		if def, ok := a.stepDef(in.CaseID, in.StepID); ok && def.Effect != nil {
			a.reconcileIntent(in, def)
		}
	}
}

func (a *App) stepDef(caseID, stepID string) (StepDef, bool) {
	c, ok := a.Case(caseID)
	if !ok {
		return StepDef{}, false
	}
	p, ok := a.PackageByName(c.Package)
	if !ok {
		return StepDef{}, false
	}
	for _, s := range p.Manifest.Steps {
		if s.ID == stepID {
			return s, true
		}
	}
	return StepDef{}, false
}

// Evaluate runs every step of a case that has become runnable.
func (a *App) Evaluate(caseID string) {
	c, ok := a.Case(caseID)
	if !ok || c.Status != core.CaseOpen {
		return
	}
	if a.Blocked(caseID) {
		return
	}
	p, ok := a.PackageByName(c.Package)
	if !ok || !p.Active {
		return
	}
	for _, def := range p.Manifest.Steps {
		st := c.Steps[def.ID]
		if st == nil || st.Status != core.StepPending {
			continue
		}
		if !a.depsMet(c, p, def) {
			continue
		}
		if def.At == "lwd" && c.BoundLWD != "" {
			if fdate(time.Now()) < c.BoundLWD {
				continue // the cut-off has not arrived
			}
		}
		a.startStep(c, def)
		// Re-read the case so later steps see fresh state this pass.
		c, _ = a.Case(caseID)
		if c == nil || c.Status != core.CaseOpen || a.Blocked(caseID) {
			return
		}
	}
	a.maybeCloseCase(caseID)
}

func (a *App) depsMet(c *core.Case, p *Package, def StepDef) bool {
	byID := map[string]StepDef{}
	for _, s := range p.Manifest.Steps {
		byID[s.ID] = s
	}
	for _, dep := range def.DependsOn {
		ds := c.Steps[dep]
		if ds == nil {
			return false
		}
		// A skipped step satisfies a dependency only when the step was
		// optional by design (the exit survey) — a cancelled mandatory
		// step can never unlock what depended on it.
		if ds.Status == core.StepSkipped && byID[dep].Optional {
			continue
		}
		if ds.Status != core.StepDone {
			return false
		}
	}
	return true
}

func (a *App) setStep(caseID, stepID string, status core.StepStatus, actor string, humanTouch bool, evidence, note string) {
	_ = a.emit(core.Receipt{
		CaseID: caseID, StepID: stepID, Action: "step.status",
		ActorType: actorType(actor), ActorID: actorID(actor), Evidence: evidence,
		Data: map[string]any{"status": string(status), "actor": actor,
			"human_touch": humanTouch, "note": note},
	})
}

func actorType(actor string) string {
	if strings.HasPrefix(actor, "agent:") || actor == "" {
		return "agent"
	}
	if strings.HasPrefix(actor, "system") {
		return "system"
	}
	return "human"
}

func actorID(actor string) string {
	return strings.TrimPrefix(actor, "agent:")
}

func (a *App) markStepDone(caseID string, def StepDef, actor string, humanTouch bool, evidence, note string) {
	a.setStep(caseID, def.ID, core.StepDone, actor, humanTouch, evidence, note)
	a.Evaluate(caseID)
}

func (a *App) startStep(c *core.Case, def StepDef) {
	switch def.Kind {
	case core.StepAgent, core.StepRule:
		if def.Impl == "close_case" {
			a.maybeCloseCase(c.ID)
			return
		}
		a.setStep(c.ID, def.ID, core.StepRunning, "agent:"+def.Impl, false, "", "")
		a.runImpl(c.ID, def)
	case core.StepEffect:
		a.setStep(c.ID, def.ID, core.StepRunning, "agent:dispatcher", false, "", "")
		cc, _ := a.Case(c.ID)
		a.runEffect(cc, def)
	case core.StepApproval:
		a.createApproval(c, def)
	case core.StepHumanInput:
		a.createTask(c, def)
	}
}

// ---------------------------------------------------------------- approvals

func (a *App) createApproval(c *core.Case, def StepDef) {
	emp, _ := a.Employee(c.EmployeeID)
	rdef, _ := a.ReservedByID(def.Reserved)
	approver := a.resolveApprover(emp, rdef)
	id := fmt.Sprintf("AP-%04d", a.next("approval"))

	req, facts := a.approvalRequest(c, def, emp)
	ap := core.Approval{
		ID: id, CaseID: c.ID, StepID: def.ID, ReservedID: def.Reserved,
		ApproverID: approver, Status: core.ApprovalOpen, Request: req, Facts: facts,
		Created: time.Now().UTC(),
	}
	_ = a.emit(core.Receipt{
		CaseID: c.ID, StepID: def.ID, Action: "approval.created", ActorType: "agent",
		ActorID: "resolver",
		Data: map[string]any{"approval": ap, "request_hash": core.MustHashCanon(req),
			"approver": approver, "reserved": def.Reserved},
	})
	a.setStep(c.ID, def.ID, core.StepWaiting, "", false, "",
		fmt.Sprintf("waiting for %s (%s)", a.userName(approver), def.Reserved))
}

// resolveApprover picks the named person from the org chart — never the
// leaver, never an agent, with escalation to the next manager if needed.
func (a *App) resolveApprover(emp core.Employee, rdef ReservedDef) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch rdef.ApproverRole {
	case "manager":
		mid := emp.ManagerID
		for hop := 0; hop < 3 && mid != ""; hop++ {
			if mid != emp.ID {
				for _, u := range a.users {
					if u.EmpID == mid && u.Role == "manager" {
						return u.ID
					}
				}
			}
			m, ok := a.employees[mid]
			if !ok {
				break
			}
			mid = m.ManagerID
		}
	default:
		for _, u := range a.users {
			if u.Role == rdef.ApproverRole {
				return u.ID
			}
		}
	}
	return ""
}

func (a *App) userName(id string) string {
	if u, ok := a.User(id); ok {
		return u.Name
	}
	return id
}

func (a *App) employeeName(id string) string {
	if e, ok := a.Employee(id); ok {
		return fmt.Sprintf("%s (%s)", e.Name, id)
	}
	return id
}

// approvalRequest builds the exact request envelope the approver decides.
// Its canonical hash is what the mandate will bind.
func (a *App) approvalRequest(c *core.Case, def StepDef, emp core.Employee) (map[string]any, []core.Fact) {
	switch def.Reserved {
	case "RSV-1":
		nf, _ := asJSON[NoticeFacts](c.Facts["notice"])
		lwd := nf.RequestedLWD
		if lwd == "" {
			lwd = nf.EarliestLWD
		}
		buyout := nf.ShortfallDays > 0
		amt := nf.BuyoutAmount
		if !buyout {
			amt = 0
		}
		req := Envelope("separation.accept", map[string]any{
			"employee_id": emp.ID, "last_working_day": lwd,
			"notice_buyout": buyout, "buyout_amount_inr": amt,
		})
		facts := []core.Fact{
			{Label: "Employee", Value: fmt.Sprintf("%s (%s, %s)", emp.Name, emp.ID, emp.Title), Source: "HR record"},
			{Label: "Resignation tendered", Value: FDisplay(c.TenderDate), Source: "chat intake, confirmed by the employee"},
			{Label: "Notice period", Value: fmt.Sprintf("%d days (%s)", nf.NoticeDays, nf.Cite), Source: "rule pack"},
			{Label: "Earliest last working day by notice", Value: FDisplay(nf.EarliestLWD), Source: "rule pack"},
			{Label: "Requested last working day", Value: FDisplay(nf.RequestedLWD), Source: "employee request"},
		}
		if buyout {
			facts = append(facts, core.Fact{Label: "Notice shortfall", Value: fmt.Sprintf("%d days → buyout ₹%.0f", nf.ShortfallDays, nf.BuyoutAmount), Source: "rule pack"})
		}
		if rec, ok := c.Facts["records"].(map[string]any); ok {
			facts = append(facts, core.Fact{Label: "Records check", Value: str(rec, "summary"), Source: "IT, Admin and payroll stand-ins (read)"})
		}
		return req, facts
	case "RSV-2":
		req := Envelope("offer.extend", map[string]any{
			"candidate_name": emp.Name, "email": emp.Email,
			"title": factStr(c, "offer_title"), "monthly_gross_inr": factFloat(c, "offer_gross_inr"),
			"joining_date": factStr(c, "joining_date"), "manager_id": factStr(c, "manager_id"),
		})
		facts := []core.Fact{
			{Label: "Candidate", Value: fmt.Sprintf("%s (%s)", emp.Name, emp.Email), Source: "offer record, entered by HR"},
			{Label: "Role and compensation", Value: fmt.Sprintf("%s · ₹%.0f gross/month", factStr(c, "offer_title"), factFloat(c, "offer_gross_inr")), Source: "offer record"},
			{Label: "Joining date", Value: FDisplay(factStr(c, "joining_date")), Source: "offer record"},
			{Label: "Reports to", Value: a.employeeName(factStr(c, "manager_id")), Source: "org chart"},
		}
		if rec, ok := c.Facts["records"].(map[string]any); ok {
			if sum := str(rec, "summary"); sum != "" {
				facts = append(facts, core.Fact{Label: "Duplicate check", Value: sum, Source: "directory stand-in (read)"})
			}
		}
		return req, facts
	case "RSV-6":
		ff, _ := asJSON[FFFacts](c.Facts["ff"])
		req := Envelope("payroll.release", map[string]any{
			"employee_id": emp.ID, "payroll_id": emp.PayrollID,
			"amount_inr": ff.Total, "period": ff.Period, "type": "full_and_final",
		})
		facts := []core.Fact{
			{Label: "Employee", Value: fmt.Sprintf("%s (%s)", emp.Name, emp.ID), Source: "HR record"},
			{Label: "Final settlement total", Value: fmt.Sprintf("₹%.2f for %s", ff.Total, ff.Period), Source: "rules, each line cited"},
		}
		for _, l := range ff.Lines {
			facts = append(facts, core.Fact{Label: l.Label, Value: fmt.Sprintf("₹%.2f", l.Amount), Source: l.Cite})
		}
		facts = append(facts, core.Fact{Label: "Staged draft", Value: "held in the payroll stand-in, not payable until this sign-off", Source: "payroll stand-in"})
		return req, facts
	}
	return map[string]any{"action": "unknown"}, nil
}

// DecideApproval is called from a human session only. The PIN re-entry is
// the demo stand-in for the IdP re-authentication with nonce = request
// hash. On approve it mints the mandate — the only place mandates exist.
func (a *App) DecideApproval(approvalID, userID, pin string, approve bool, edits map[string]any, reason string) error {
	ap, ok := a.Approval(approvalID)
	if !ok {
		return fmt.Errorf("unknown approval")
	}
	if ap.Status != core.ApprovalOpen {
		return fmt.Errorf("already decided")
	}
	u, ok := a.User(userID)
	if !ok || u.ID != ap.ApproverID {
		return fmt.Errorf("only the named approver (%s) may decide this", a.userName(ap.ApproverID))
	}
	if u.PIN != pin {
		return fmt.Errorf("re-authentication failed: wrong PIN")
	}
	// Separation of duties: whoever opened the case cannot also decide
	// its reserved approvals — the warrant needs a second named person.
	for _, rr := range a.Receipts(0, ap.CaseID) {
		if rr.Action == "case.opened" {
			if rr.ActorType == "human" && rr.ActorID == userID {
				return fmt.Errorf("REFUSED: separation of duties — you opened this case, so a different named person must decide %s", ap.ReservedID)
			}
			break
		}
	}
	req := map[string]any{}
	for k, v := range ap.Request {
		req[k] = v
	}
	args, _ := req["args"].(map[string]any)
	if args == nil {
		args = map[string]any{}
		req["args"] = args
	}
	// The approver may adjust the bound terms before approving (for
	// RSV-1: the release date and buyout). What they approve is what
	// binds — edits change the hash, and the hash is the mandate.
	if approve && ap.ReservedID == "RSV-1" {
		if v := str(edits, "last_working_day"); v != "" {
			if _, err := parseDate(v); err != nil {
				return fmt.Errorf("release date: %w", err)
			}
			args["last_working_day"] = v
		}
		if v, ok := edits["notice_buyout"]; ok {
			args["notice_buyout"] = v == "on" || v == true
		}
		if args["notice_buyout"] == false {
			args["buyout_amount_inr"] = 0.0
		}
	}
	status := core.ApprovalRejected
	if approve {
		status = core.ApprovalGranted
	}
	if err := a.emit(core.Receipt{
		CaseID: ap.CaseID, StepID: ap.StepID, Action: "approval.decided", ActorType: "human",
		ActorID: userID,
		Data: map[string]any{"approval_id": ap.ID, "status": string(status),
			"request": req, "request_hash": core.MustHashCanon(req), "reason": reason,
			"re_auth": "demo-pin (stand-in for IdP re-login, nonce = request hash)"},
	}); err != nil {
		return err
	}
	if !approve {
		a.setStep(ap.CaseID, ap.StepID, core.StepDone, userID, true, "", "rejected: "+reason)
		a.openInDoubt(ap.CaseID, ap.StepID, "", "hrops",
			fmt.Sprintf("%s rejected by %s: %s", ap.ReservedID, a.userName(userID), reason), nil)
		return nil
	}
	m := core.Mandate{
		ID: fmt.Sprintf("MD-%04d", a.next("mandate")), ApprovalID: ap.ID, CaseID: ap.CaseID,
		ReservedID: ap.ReservedID, ApproverID: userID,
		RequestHash: core.MustHashCanon(req),
		IssuedAt:    time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(45 * time.Minute),
		SingleUse: true, ReAuth: "demo-pin",
	}
	if err := a.emit(core.Receipt{
		CaseID: ap.CaseID, StepID: ap.StepID, Action: "mandate.issued", ActorType: "human",
		ActorID: userID,
		Data:    map[string]any{"mandate": m},
	}); err != nil {
		return err
	}
	// Apply the approved terms to the case, then release the effect the
	// mandate covers (if the step carries one) — the dispatcher recomputes
	// the hash over the outgoing bytes, so what runs is what was signed.
	if ap.ReservedID == "RSV-1" {
		_ = a.emit(core.Receipt{
			CaseID: ap.CaseID, Action: "case.updated", ActorType: "human", ActorID: userID,
			Data: map[string]any{"bound_lwd": str(args, "last_working_day"),
				"buyout": args["notice_buyout"] == true,
				"facts":  map[string]any{"buyout_amount": args["buyout_amount_inr"]}},
		})
	}
	def := a.mustStepDef(ap.CaseID, ap.StepID)
	if def.Effect != nil {
		a.setStep(ap.CaseID, ap.StepID, core.StepRunning, userID, true, "",
			ap.ReservedID+" granted; dispatching under the mandate")
		cc, _ := a.Case(ap.CaseID)
		go a.runEffect(cc, def)
		return nil
	}
	a.markStepDone(ap.CaseID, def, userID, true, "", ap.ReservedID+" granted")
	return nil
}

func (a *App) mustStepDef(caseID, stepID string) StepDef {
	def, _ := a.stepDef(caseID, stepID)
	return def
}

// ---------------------------------------------------------------- tasks

func (a *App) createTask(c *core.Case, def StepDef) {
	emp, _ := a.Employee(c.EmployeeID)
	t := core.Task{
		ID: fmt.Sprintf("TK-%04d", a.next("task")), CaseID: c.ID, StepID: def.ID,
		Role: def.Role, Title: fmt.Sprintf("%s — %s", def.Title, emp.Name),
		Open: true, Created: time.Now().UTC(),
	}
	if def.Role == "leaver" {
		for _, u := range a.Users() {
			if u.EmpID == emp.ID {
				t.UserID = u.ID
			}
		}
	}
	if def.Role == "manager" {
		rdef := ReservedDef{ApproverRole: "manager"}
		t.UserID = a.resolveApprover(emp, rdef)
	}
	_ = a.emit(core.Receipt{
		CaseID: c.ID, StepID: def.ID, Action: "task.created", ActorType: "agent",
		ActorID: "engine", Data: map[string]any{"task": t},
	})
	a.setStep(c.ID, def.ID, core.StepWaiting, "", false, "", "waiting: "+def.Title)
}

func (a *App) CompleteTask(taskID, userID string, note string) error {
	a.mu.Lock()
	t, ok := a.tasks[taskID]
	if !ok || !t.Open {
		a.mu.Unlock()
		return fmt.Errorf("task is not open")
	}
	tt := *t
	a.mu.Unlock()
	u, _ := a.User(userID)
	if tt.UserID != "" && tt.UserID != userID {
		return fmt.Errorf("this task belongs to %s", a.userName(tt.UserID))
	}
	if tt.UserID == "" && u.Role != tt.Role {
		return fmt.Errorf("this task belongs to the %s role", tt.Role)
	}
	if err := a.emit(core.Receipt{
		CaseID: tt.CaseID, StepID: tt.StepID, Action: "task.done", ActorType: "human",
		ActorID: userID, Data: map[string]any{"task_id": tt.ID, "note_hash": core.Hash([]byte(note))},
	}); err != nil {
		return err
	}
	a.markStepDone(tt.CaseID, a.mustStepDef(tt.CaseID, tt.StepID), userID, true, "", "")
	return nil
}

// SkipTask marks an optional human input (the exit survey) declined. The
// same ownership rule as CompleteTask applies: only the task's own person
// (or its role, when unassigned) may decline it.
func (a *App) SkipTask(taskID, userID string) error {
	a.mu.Lock()
	t, ok := a.tasks[taskID]
	if !ok || !t.Open {
		a.mu.Unlock()
		return fmt.Errorf("task is not open")
	}
	tt := *t
	a.mu.Unlock()
	u, _ := a.User(userID)
	if tt.UserID != "" && tt.UserID != userID {
		return fmt.Errorf("this task belongs to %s", a.userName(tt.UserID))
	}
	if tt.UserID == "" && u.Role != tt.Role {
		return fmt.Errorf("this task belongs to the %s role", tt.Role)
	}
	def := a.mustStepDef(tt.CaseID, tt.StepID)
	if !def.Optional {
		return fmt.Errorf("this step cannot be skipped")
	}
	if err := a.emit(core.Receipt{
		CaseID: tt.CaseID, StepID: tt.StepID, Action: "task.done", ActorType: "human",
		ActorID: userID, Data: map[string]any{"task_id": tt.ID, "skipped": true},
	}); err != nil {
		return err
	}
	a.setStep(tt.CaseID, tt.StepID, core.StepSkipped, userID, true, "", "declined")
	a.Evaluate(tt.CaseID)
	return nil
}

// ---------------------------------------------------------------- in doubt

func (a *App) openInDoubt(caseID, stepID, intentKey, ownerRole, reason string, ev []core.Fact) {
	it := core.InDoubtItem{
		ID: fmt.Sprintf("ID-%04d", a.next("indoubt")), CaseID: caseID, StepID: stepID,
		IntentKey: intentKey, OwnerRole: ownerRole, Reason: reason, Evidence: ev,
		Open: true, Created: time.Now().UTC(),
	}
	_ = a.emit(core.Receipt{
		CaseID: caseID, StepID: stepID, Action: "indoubt.opened", ActorType: "agent",
		ActorID: "verifier", Data: map[string]any{"item": it, "reason": reason},
	})
	a.setStep(caseID, stepID, core.StepInDoubt, "", false, "", reason)
}

// ResolveInDoubt lets the named owner close an item after looking at the
// target system: confirm done (with what they saw), or send again.
func (a *App) ResolveInDoubt(itemID, userID, resolution, note string) error {
	a.mu.Lock()
	it, ok := a.indoubt[itemID]
	if !ok || !it.Open {
		a.mu.Unlock()
		return fmt.Errorf("item is not open")
	}
	item := *it
	a.mu.Unlock()
	u, ok := a.User(userID)
	if !ok || (u.Role != item.OwnerRole && u.Role != "hrops") {
		return fmt.Errorf("this item belongs to the %s role", item.OwnerRole)
	}
	def := a.mustStepDef(item.CaseID, item.StepID)
	// A reserved decision can never be closed or cancelled by hand from
	// the in-doubt queue: that would be a person other than the named
	// approver completing it. The only way forward is a re-dispatch under
	// the mandate (or a fresh approval); the refusal itself is the guard.
	reserved := def.Kind == core.StepApproval || def.Reserved != ""
	if !reserved && def.Effect != nil {
		_, reserved = a.Reserved(def.Effect.Action)
	}
	if reserved && resolution != "retry" {
		return fmt.Errorf("REFUSED: step %s is reserved (%s) — it can only be re-dispatched under its mandate, never closed or cancelled by hand", def.ID, def.Reserved)
	}
	if err := a.emit(core.Receipt{
		CaseID: item.CaseID, StepID: item.StepID, Action: "indoubt.resolved", ActorType: "human",
		ActorID: userID, Data: map[string]any{"item_id": item.ID, "resolution": resolution,
			"note": note},
	}); err != nil {
		return err
	}
	switch resolution {
	case "confirmed_done":
		// A person looked at the target and confirmed it. Human evidence.
		if item.IntentKey != "" {
			_, _ = a.ledger.Transition(item.IntentKey, func(x *core.Intent) {
				x.State = core.IntentCommitted
				x.Evidence = core.EvObserved
				x.Note = "confirmed by " + userID + ": " + note
			})
		}
		a.markStepDone(item.CaseID, def, userID, true, core.EvObserved, "confirmed by a person: "+note)
	case "retry":
		if def.Effect == nil {
			return fmt.Errorf("nothing to retry on this step")
		}
		_ = a.emit(core.Receipt{CaseID: item.CaseID, StepID: item.StepID, Action: "effect.retried",
			ActorType: "human", ActorID: userID,
			Data: map[string]any{"intent_key": item.IntentKey, "note": "manual retry ordered from the in-doubt queue"}})
		a.setStep(item.CaseID, item.StepID, core.StepRunning, "agent:dispatcher", true, "", "retry ordered by "+userID)
		if item.IntentKey != "" {
			st, _ := a.ledger.Transition(item.IntentKey, func(x *core.Intent) {
				x.State = core.IntentPending
				x.Attempts++
				x.Note = "manual retry by " + userID
			})
			go func() {
				out := a.sendAndReadBack(st, def, true)
				a.settleOutcome(item.CaseID, def, out)
			}()
		} else {
			cc, _ := a.Case(item.CaseID)
			go a.runEffect(cc, def)
		}
	case "cancelled":
		a.setStep(item.CaseID, item.StepID, core.StepSkipped, userID, true, "", "cancelled: "+note)
		a.Evaluate(item.CaseID)
	default:
		return fmt.Errorf("unknown resolution")
	}
	return nil
}

// ---------------------------------------------------------------- intake

// StartResignation opens a case from the employee's confirmed summary. The
// confirmation click is the tender; a model never decides the date.
func (a *App) StartResignation(empID, requestedLWD string) (string, error) {
	emp, ok := a.Employee(empID)
	if !ok {
		return "", fmt.Errorf("unknown employee")
	}
	if c, ok := a.CaseForEmployee(empID); ok && c.Status == core.CaseOpen {
		return c.ID, fmt.Errorf("an exit case is already open for %s", emp.Name)
	}
	p, ok := a.PackageByName("exits")
	if !ok || !p.Active {
		return "", fmt.Errorf("the exits package is not active")
	}
	id := fmt.Sprintf("EXIT-%04d", a.next("case"))
	c := core.Case{
		ID: id, Package: "exits", EmployeeID: empID, Status: core.CaseOpen,
		OpenedAt: time.Now().UTC(), TenderDate: fdate(time.Now()),
		RequestedLWD: requestedLWD, Steps: map[string]*core.StepState{},
	}
	for _, s := range p.Manifest.Steps {
		c.Steps[s.ID] = &core.StepState{ID: s.ID, N: s.N, Status: core.StepPending}
	}
	if err := a.emit(core.Receipt{
		CaseID: id, Action: "case.opened", ActorType: "human", ActorID: empID,
		Data: map[string]any{"case": c, "note": "resignation tendered in web chat; summary confirmed by the employee"},
	}); err != nil {
		return "", err
	}
	// The agent executes intake; the employee's confirm click is its
	// input (the tender), so the step itself counts as automated.
	a.setStep(id, "intake", core.StepDone, "agent:intake", false, "",
		"tender confirmed by the employee in chat")
	a.Evaluate(id)
	return id, nil
}

// ---------------------------------------------------------------- rule & agent impls

func (a *App) runImpl(caseID string, def StepDef) {
	c, _ := a.Case(caseID)
	if c == nil {
		return
	}
	emp, _ := a.Employee(c.EmployeeID)
	fail := func(err error) {
		a.openInDoubt(caseID, def.ID, "", "hrops", def.Impl+": "+err.Error(), nil)
	}
	update := func(facts map[string]any) {
		_ = a.emit(core.Receipt{
			CaseID: caseID, StepID: def.ID, Action: "case.updated", ActorType: "agent",
			ActorID: def.Impl, Data: map[string]any{"facts": facts},
		})
	}
	onboarding := c.Package == "onboarding"
	switch def.Impl {
	case "records_check":
		if onboarding {
			// Pre-hire guard: the directory must NOT already know this
			// address. Verified against the target's own records.
			ok, sum, factsMap := a.onboardingRecordsCheck(emp)
			update(map[string]any{"records": factsMap})
			if !ok {
				a.openInDoubt(caseID, def.ID, "", "hrops", sum, nil)
				return
			}
			a.markStepDone(caseID, def, "agent:records_check", false, "", sum)
			return
		}
		sum, factsMap := a.recordsCheck(emp)
		update(map[string]any{"records": factsMap})
		a.markStepDone(caseID, def, "agent:records_check", false, "", sum)
	case "disclosure_screen":
		// POSH / grievance keywords route only; the text itself is sealed
		// (kept out of receipts; a hash stands in).
		text := factStr(c, "intake_text")
		flag := containsAny(strings.ToLower(text), []string{"harass", "posh", "grievance", "whistle", "complaint"})
		if flag {
			_ = a.emit(core.Receipt{CaseID: caseID, StepID: def.ID, Action: "disclosure.routed",
				ActorType: "agent", ActorID: def.Impl,
				Data: map[string]any{"routed_to": "ICC / grievance officer", "text_hash": core.Hash([]byte(text)),
					"note": "route only; Counterfoil never decides these (RSV-5)"}})
			update(map[string]any{"sealed": true})
			_ = a.emit(core.Receipt{CaseID: caseID, Action: "case.updated", ActorType: "agent",
				ActorID: def.Impl, Data: map[string]any{"sealed": true}})
		}
		a.markStepDone(caseID, def, "agent:disclosure_screen", false, "",
			map[bool]string{true: "disclosure routed, text sealed", false: "no disclosure found"}[flag])
	case "notice_calc":
		nf, err := a.rules.ComputeNotice(emp, c.TenderDate, c.RequestedLWD)
		if err != nil {
			fail(err)
			return
		}
		update(map[string]any{"notice": nf})
		a.markStepDone(caseID, def, "agent:notice_calc", false, "",
			fmt.Sprintf("notice %dd; earliest LWD %s", nf.NoticeDays, nf.EarliestLWD))
	case "approval_pack":
		// The fact sheet is assembled by code from typed facts. A model
		// may later phrase it; it can never add or change a number.
		a.markStepDone(caseID, def, "agent:approval_pack", false, "", "fact sheet assembled from typed facts")
	case "confirm_clocks":
		ffEligible := false
		if ff, ok := asJSON[FFFacts](c.Facts["ff"]); ok {
			ffEligible = ff.GratuityEligible
		} else {
			join, err := parseDate(emp.JoinDate)
			if err == nil && c.BoundLWD != "" {
				if lwd, err := parseDate(c.BoundLWD); err == nil {
					years := lwd.Sub(join).Hours() / 24 / 365.25
					ffEligible = years >= a.rules.Gratuity.MinYears
				}
			}
		}
		if err := a.startClocks(caseID, emp, c.BoundLWD, ffEligible); err != nil {
			fail(err)
			return
		}
		a.notifyEmployee(c, emp)
		a.markStepDone(caseID, def, "agent:confirm_clocks", false, "",
			"employee informed; statutory clocks started from "+c.BoundLWD)
	case "clearance":
		if onboarding {
			okAll, sum := a.day1Check(c)
			update(map[string]any{"day1": sum})
			if !okAll {
				a.openInDoubt(caseID, def.ID, "", "hrops", sum, nil)
				return
			}
			a.markStepDone(caseID, def, "agent:day1_check", false, core.EvAttributed, sum)
			return
		}
		okAll, sum := a.clearanceCheck(c, emp)
		update(map[string]any{"clearance": sum})
		if !okAll {
			a.openInDoubt(caseID, def.ID, "", "hrops", "clearance re-check failed: "+sum, nil)
			return
		}
		a.markStepDone(caseID, def, "agent:clearance", false, core.EvAttributed, sum)
	case "ff_compute":
		buyout := c.Buyout
		nf, _ := asJSON[NoticeFacts](c.Facts["notice"])
		buyAmt := factFloat(c, "buyout_amount")
		if buyAmt == 0 {
			buyAmt = nf.BuyoutAmount
		}
		ff, err := a.rules.ComputeFF(emp, c.BoundLWD, buyout, buyAmt)
		if err != nil {
			fail(err)
			return
		}
		update(map[string]any{"ff": ff, "ff_total": ff.Total, "ff_period": ff.Period})
		a.markStepDone(caseID, def, "agent:ff_compute", false, "",
			fmt.Sprintf("final settlement ₹%.2f (%d lines, each cited)", ff.Total, len(ff.Lines)))
	case "deadline_monitor":
		// Completes when every clock for the case is closed. A journey
		// that declares no clocks has nothing to watch — done, and says so.
		obs := a.ObligationsFor(caseID)
		open := 0
		for _, ob := range obs {
			if ob.Status != core.ClockClosed {
				open++
			}
		}
		switch {
		case len(obs) == 0:
			a.markStepDone(caseID, def, "agent:deadline_monitor", false, "",
				"no statutory clocks declared for this journey")
		case open == 0:
			a.markStepDone(caseID, def, "agent:deadline_monitor", false, "", "every clock closed on evidence")
		default:
			a.setStep(caseID, def.ID, core.StepRunning, "agent:deadline_monitor", false, "",
				fmt.Sprintf("%d clock(s) still open", open))
		}
	}
}

func containsAny(s string, keys []string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func (a *App) notifyEmployee(c *core.Case, emp core.Employee) {
	lwd := FDisplay(c.BoundLWD)
	msg := fmt.Sprintf("Your resignation is accepted. Last working day: %s.", lwd)
	if c.Buyout {
		msg += fmt.Sprintf(" The notice shortfall is bought out (₹%.0f, recovered in your final settlement).", factFloat(c, "buyout_amount"))
	}
	msg += " Your final wages clock has started: only the payroll system's own record can close it."
	a.AddChat(ChatMsg{CaseID: c.ID, From: "counterfoil", Text: msg})
	_ = a.emit(core.Receipt{CaseID: c.ID, Action: "message.sent", ActorType: "agent",
		ActorID: "notifier", Data: map[string]any{"to": emp.ID, "text_hash": core.Hash([]byte(msg)),
			"channel": "web chat"}})
}

// recordsCheck reads the target stand-ins (reads only) and reconciles them
// against the HR record.
func (a *App) recordsCheck(emp core.Employee) (string, map[string]any) {
	facts := map[string]any{}
	var notes []string
	if rec, err := a.mockGet("/directory/users/" + emp.Email); err == nil {
		facts["directory_status"] = str(rec, "status")
		notes = append(notes, "directory: "+str(rec, "status"))
	} else {
		notes = append(notes, "directory: unreachable")
	}
	if rec, err := a.mockGet("/cards/cards/" + emp.CardID); err == nil {
		facts["card_status"] = str(rec, "status")
		notes = append(notes, "card "+emp.CardID+": "+str(rec, "status"))
	} else {
		notes = append(notes, "card: unreachable")
	}
	facts["assets"] = strings.Join(emp.Assets, ", ")
	notes = append(notes, "assets on record: "+strings.Join(emp.Assets, ", "))
	sum := strings.Join(notes, " · ")
	facts["summary"] = sum
	return sum, facts
}

func (a *App) clearanceCheck(c *core.Case, emp core.Employee) (bool, string) {
	okAll := true
	var notes []string
	if rec, err := a.mockGet("/directory/users/" + emp.Email); err == nil && str(rec, "status") == "disabled" {
		notes = append(notes, "directory disabled ✓")
	} else {
		okAll = false
		notes = append(notes, "directory NOT disabled")
	}
	if rec, err := a.mockGet("/cards/cards/" + emp.CardID); err == nil && str(rec, "status") == "revoked" {
		notes = append(notes, "card revoked ✓")
	} else {
		okAll = false
		notes = append(notes, "card NOT revoked")
	}
	return okAll, strings.Join(notes, " · ")
}

func (a *App) maybeCloseCase(caseID string) {
	c, ok := a.Case(caseID)
	if !ok || c.Status != core.CaseOpen {
		return
	}
	p, _ := a.PackageByName(c.Package)
	if p == nil {
		return
	}
	for _, s := range p.Manifest.Steps {
		if s.ID == "close" {
			continue
		}
		st := c.Steps[s.ID]
		if st == nil {
			return
		}
		if st.Status == core.StepDone || (st.Status == core.StepSkipped && s.Optional) {
			continue
		}
		return
	}
	// Leaver-closure statement, computed from receipts.
	issued, revoked, probes := 0, 0, 0
	for _, r := range a.Receipts(0, caseID) {
		switch r.Action {
		case "effect.readback":
			revoked++
		case "effect.refused":
			probes++
		case "document.issued", "payment.released":
			issued++
		}
	}
	st := c.Steps["close"]
	if st != nil && st.Status == core.StepPending {
		_ = a.emit(core.Receipt{CaseID: caseID, StepID: "close", Action: "case.closed",
			ActorType: "agent", ActorID: "kernel",
			Data: map[string]any{"statement": fmt.Sprintf(
				"%d effects issued or released, %d confirmed by read-back, %d refusals on record",
				issued, revoked, probes), "retention": "exit-records, 8 years (policy)"}})
		a.setStep(caseID, "close", core.StepDone, "agent:kernel", false, "internal", "case closed")
	}
}

// StepDefs returns the ordered step definitions for a case's package.
func (a *App) StepDefs(c *core.Case) []StepDef {
	p, ok := a.PackageByName(c.Package)
	if !ok {
		return nil
	}
	defs := make([]StepDef, len(p.Manifest.Steps))
	copy(defs, p.Manifest.Steps)
	sort.Slice(defs, func(i, j int) bool { return defs[i].N < defs[j].N })
	return defs
}
