// The second service line, live: onboarding runs on the same kernel —
// same gate, same ledger, same read-back, same receipts — proving the
// platform claim with a journey, not a sentence. Opening a case here is
// refused until the package has passed `cfoil check` and been activated
// by two named people.
package kernel

import (
	"fmt"
	"strings"
	"time"

	"counterfoil/internal/core"
)

// OfferInput is what HR enters from a signed offer. Typed, validated —
// a model never fills these.
type OfferInput struct {
	Name         string
	Email        string
	Title        string
	ManagerID    string
	Location     string
	JoiningDate  string // YYYY-MM-DD
	MonthlyGross float64
}

// StartOnboarding mints the candidate's employee record and opens the
// onboarding case. byUserID is the HR person entering the offer; their
// name goes on the intake receipt.
func (a *App) StartOnboarding(byUserID string, in OfferInput) (string, error) {
	p, ok := a.PackageByName("onboarding")
	if !ok {
		return "", fmt.Errorf("no onboarding package on this instance")
	}
	if !p.Active {
		return "", fmt.Errorf("REFUSED: the onboarding package is not active — run its admission check and activate it with two named people (Controls & drills)")
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Title = strings.TrimSpace(in.Title)
	if in.Name == "" || in.Email == "" || in.Title == "" {
		return "", fmt.Errorf("name, email and title are required")
	}
	if !strings.Contains(in.Email, "@") {
		return "", fmt.Errorf("email does not look like an address")
	}
	if in.MonthlyGross <= 0 {
		return "", fmt.Errorf("monthly gross must be a positive amount")
	}
	jd, err := parseDate(in.JoiningDate)
	if err != nil {
		return "", fmt.Errorf("joining date: %w", err)
	}
	if jd.Before(time.Now().Truncate(24 * time.Hour)) {
		return "", fmt.Errorf("joining date %s is in the past", in.JoiningDate)
	}
	if _, ok := a.Employee(in.ManagerID); !ok {
		return "", fmt.Errorf("unknown manager %q", in.ManagerID)
	}
	for _, e := range a.Employees() {
		if strings.EqualFold(e.Email, in.Email) {
			return "", fmt.Errorf("an employee record already holds %s (%s)", in.Email, e.ID)
		}
	}

	n := a.next("employee")
	emp := core.Employee{
		ID: fmt.Sprintf("EMP-%04d", 2000+n), Name: in.Name, Email: in.Email,
		Title: in.Title, ManagerID: in.ManagerID, Location: in.Location,
		JoinDate: in.JoiningDate, MonthlyGross: in.MonthlyGross, BasicShare: 0.4,
		PayrollID: fmt.Sprintf("PR-%04d", 2000+n),
	}
	if err := a.emit(core.Receipt{
		Action: "employee.created", ActorType: "human", ActorID: byUserID,
		Data: map[string]any{"employee": emp,
			"note": "candidate record minted from the signed offer, entered by " + a.userName(byUserID)},
	}); err != nil {
		return "", err
	}

	id := fmt.Sprintf("ONB-%04d", a.next("case"))
	c := core.Case{
		ID: id, Package: "onboarding", EmployeeID: emp.ID, Status: core.CaseOpen,
		OpenedAt: time.Now().UTC(), Steps: map[string]*core.StepState{},
		Facts: map[string]any{
			"offer_title": in.Title, "offer_gross_inr": in.MonthlyGross,
			"joining_date": in.JoiningDate, "manager_id": in.ManagerID,
		},
	}
	for _, s := range p.Manifest.Steps {
		c.Steps[s.ID] = &core.StepState{ID: s.ID, N: s.N, Status: core.StepPending}
	}
	if err := a.emit(core.Receipt{
		CaseID: id, Action: "case.opened", ActorType: "human", ActorID: byUserID,
		Data: map[string]any{"case": c, "note": "onboarding opened from a signed offer"},
	}); err != nil {
		return "", err
	}
	a.setStep(id, "intake", core.StepDone, "agent:intake", false, "",
		"candidate record opened from the signed offer (entered by "+a.userName(byUserID)+")")
	a.Evaluate(id)
	return id, nil
}

// onboardingRecordsCheck is the pre-hire duplicate guard: a joiner must
// NOT already hold a directory account. The absence is verified against
// the directory's own records, not assumed.
func (a *App) onboardingRecordsCheck(emp core.Employee) (ok bool, summary string, facts map[string]any) {
	facts = map[string]any{"email": emp.Email}
	rec, err := a.LiveDirectory(emp.Email)
	if err != nil {
		// The directory holds nothing for this address — that is the
		// green state for a new hire.
		sum := fmt.Sprintf("directory holds no account for %s — no duplicate hire; payroll id %s reserved",
			emp.Email, emp.PayrollID)
		facts["directory"], facts["summary"] = "no existing account", sum
		return true, sum, facts
	}
	sum := fmt.Sprintf("an account for %s already exists in the directory (status %v) — possible duplicate hire",
		emp.Email, rec["status"])
	facts["directory"], facts["summary"] = rec, sum
	return false, sum, facts
}

// day1Check re-checks day-one readiness against what each step actually
// proved: the provisioning effects must stand at their declared
// evidence. It reads the case's own step evidence — which was itself set
// by read-back against the targets.
func (a *App) day1Check(c *core.Case) (ok bool, summary string) {
	need := []string{"it_ticket", "card_issue", "payroll_setup"}
	var bad []string
	for _, id := range need {
		st := c.Steps[id]
		if st == nil || st.Status != core.StepDone || st.Evidence != core.EvAttributed {
			ev := "missing"
			if st != nil {
				ev = fmt.Sprintf("%s/%s", st.Status, st.Evidence)
			}
			bad = append(bad, id+" ("+ev+")")
		}
	}
	if len(bad) > 0 {
		return false, "not day-one ready: " + strings.Join(bad, ", ")
	}
	return true, "laptop and card tickets confirmed in IT's own records; payroll setup staged — all attributed"
}

// SubmitSurvey records a survey's answers and completes its task. The
// answers are synthetic demo content; the submission itself is the
// receipt that matters.
func (a *App) SubmitSurvey(taskID, userID string, answers map[string]string) error {
	t, ok := a.taskByID(taskID)
	if !ok || !t.Open {
		return fmt.Errorf("no open task %s", taskID)
	}
	data := map[string]any{"task_id": taskID}
	for k, v := range answers {
		if strings.TrimSpace(v) != "" {
			data["answer_"+k] = strings.TrimSpace(v)
		}
	}
	if err := a.emit(core.Receipt{
		CaseID: t.CaseID, StepID: t.StepID, Action: "survey.submitted",
		ActorType: "human", ActorID: userID, Data: data,
	}); err != nil {
		return err
	}
	return a.CompleteTask(taskID, userID, "survey submitted — answers on the receipt")
}

func (a *App) taskByID(id string) (*core.Task, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tasks[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}
