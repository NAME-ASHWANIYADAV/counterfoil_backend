package webapp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"counterfoil/internal/audit"
	"counterfoil/internal/core"
	"counterfoil/internal/kernel"
)

func (s *Server) routes() {
	m := s.mux
	m.Handle("GET /static/", http.FileServerFS(staticFS))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })

	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.login)
	m.HandleFunc("GET /logout", s.logout)
	m.HandleFunc("GET /{$}", s.home)

	m.HandleFunc("GET /chat", s.chatPage)
	m.HandleFunc("POST /chat/send", s.chatSend)
	m.HandleFunc("POST /chat/confirm", s.chatConfirm)

	m.HandleFunc("GET /work", s.workPage)
	m.HandleFunc("GET /approvals/{id}", s.approvalPage)
	m.HandleFunc("POST /approvals/{id}/decide", s.approvalDecide)
	m.HandleFunc("POST /tasks/{id}/done", s.taskDone)
	m.HandleFunc("POST /tasks/{id}/skip", s.taskSkip)

	m.HandleFunc("GET /cases", s.casesPage)
	m.HandleFunc("GET /cases/{id}", s.casePage)
	m.HandleFunc("GET /indoubt", s.indoubtPage)
	m.HandleFunc("POST /indoubt/{id}/resolve", s.indoubtResolve)

	m.HandleFunc("GET /dashboard", s.dashboardPage)

	m.HandleFunc("GET /admin", s.adminPage)
	m.HandleFunc("POST /admin/kill", s.adminKill)
	m.HandleFunc("POST /admin/lie", s.adminLie)
	m.HandleFunc("POST /admin/tamper", s.adminTamper)
	m.HandleFunc("POST /admin/canaries", s.adminCanaries)
	m.HandleFunc("POST /admin/pkg/check", s.adminPkgCheck)
	m.HandleFunc("POST /admin/pkg/activate", s.adminPkgActivate)
	m.HandleFunc("POST /admin/reserved/add", s.adminReservedAdd)

	m.HandleFunc("GET /audit", s.auditPage)
	m.HandleFunc("POST /audit/export", s.auditExport)
	m.HandleFunc("POST /audit/verify", s.auditVerify)
	m.HandleFunc("GET /letter/{id}", s.letterPage)
}

// ---------------------------------------------------------------- login

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	type vm struct{ Users []core.User }
	users := s.app.Users()
	order := map[string]int{"leaver": 0, "manager": 1, "payroll": 2, "hrops": 3, "hradmin": 4, "auditor": 5}
	sort.SliceStable(users, func(i, j int) bool { return order[users[i].Role] < order[users[j].Role] })
	s.render(w, r, "login", "", "Sign in", vm{Users: users})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	uid := r.FormValue("user")
	if _, ok := s.app.User(uid); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	sid := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[sid] = uid
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "cfoil_sid", Value: sid, Path: "/", HttpOnly: true})
	u, _ := s.app.User(uid)
	http.Redirect(w, r, homeFor(u), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delete(s.sessions, s.sid(r))
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "cfoil_sid", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	u, ok := s.user(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, homeFor(u), http.StatusSeeOther)
}

// ---------------------------------------------------------------- chat (the leaver)

type chatVM struct {
	Emp      core.Employee
	Msgs     []kernel.ChatMsg
	Case     *core.Case
	CaseVM   *caseVM
	Pending  pendingIntake
	Tasks    []*core.Task
}

type pendingIntake struct {
	Active       bool
	RequestedLWD string
}

var pendingByUser = struct {
	sync  map[string]pendingIntake
	guard chan struct{}
}{sync: map[string]pendingIntake{}, guard: make(chan struct{}, 1)}

func setPending(uid string, p pendingIntake) {
	pendingByUser.guard <- struct{}{}
	pendingByUser.sync[uid] = p
	<-pendingByUser.guard
}

func getPending(uid string) pendingIntake {
	pendingByUser.guard <- struct{}{}
	p := pendingByUser.sync[uid]
	<-pendingByUser.guard
	return p
}

func (s *Server) chatPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "leaver")
	if !ok {
		return
	}
	emp, _ := s.app.Employee(u.EmpID)
	vm := chatVM{Emp: emp, Pending: getPending(u.ID), Tasks: s.app.TasksFor("leaver", u.ID)}
	if c, ok := s.app.CaseForEmployee(u.EmpID); ok {
		vm.Case = c
		vm.Msgs = s.app.ChatFor(c.ID)
		cv := s.buildCaseVM(c, true)
		vm.CaseVM = &cv
	} else {
		vm.Msgs = s.app.ChatFor("PRE-" + u.ID)
	}
	s.render(w, r, "chat", "chat", "Chat", vm)
}

func (s *Server) chatSend(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "leaver")
	if !ok {
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	caseID := "PRE-" + u.ID
	if c, ok := s.app.CaseForEmployee(u.EmpID); ok {
		caseID = c.ID
	}
	s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: u.ID, Text: text})
	emp, _ := s.app.Employee(u.EmpID)

	// Rules first: is this a resignation, and which date?
	isRes, lwd := s.app.Router().ParseIntake(text)
	switch {
	case isRes && !strings.HasPrefix(caseID, "PRE-"):
		s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil",
			Text: "Your exit case is already open — the tracker below is live."})
	case isRes:
		disp := "not stated (the notice rule will propose the earliest date)"
		if lwd != "" {
			disp = kernel.FDisplay(lwd)
		}
		ack, src := s.app.Router().Acknowledge(emp.Name, disp)
		s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil", Text: ack,
			Kind: "src:" + src})
		setPending(u.ID, pendingIntake{Active: true, RequestedLWD: lwd})
	default:
		if qa, ok := s.app.Rules().AnswerQA(text); ok {
			s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil",
				Text: qa.Answer, Kind: "cite:" + qa.Cite})
		} else {
			s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil",
				Text: "I answer policy questions from the cited rule pack, and I can take your resignation. " +
					"Try: \"What is my notice period?\" — or tell me you wish to resign.",
				Kind: "src:rules (no matching clause)"})
		}
	}
	http.Redirect(w, r, "/chat", http.StatusSeeOther)
}

func (s *Server) chatConfirm(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "leaver")
	if !ok {
		return
	}
	p := getPending(u.ID)
	if !p.Active {
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	if r.FormValue("decision") != "confirm" {
		setPending(u.ID, pendingIntake{})
		s.app.AddChat(kernel.ChatMsg{CaseID: "PRE-" + u.ID, From: "counterfoil",
			Text: "No problem — nothing has been submitted."})
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	if v := strings.TrimSpace(r.FormValue("requested_lwd")); v != "" {
		p.RequestedLWD = v
	}
	caseID, err := s.app.StartResignation(u.EmpID, p.RequestedLWD)
	setPending(u.ID, pendingIntake{})
	if err != nil {
		s.setFlash(r, err.Error())
		http.Redirect(w, r, "/chat", http.StatusSeeOther)
		return
	}
	// Carry the pre-case transcript into the case.
	for _, m := range s.app.ChatFor("PRE-" + u.ID) {
		m.CaseID = caseID
		s.app.AddChat(m)
	}
	s.app.AddChat(kernel.ChatMsg{CaseID: caseID, From: "counterfoil",
		Text: "Submitted. Your resignation is acknowledged and the case is open — nothing is accepted until your named approver decides. You can watch every step and clock below; each line comes from a receipt."})
	http.Redirect(w, r, "/chat", http.StatusSeeOther)
}

// ---------------------------------------------------------------- work (approvals + tasks)

type workVM struct {
	Approvals []*core.Approval
	CaseOf    map[string]*core.Case
	EmpOf     map[string]core.Employee
	Tasks     []*core.Task
}

func (s *Server) workPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	vm := workVM{CaseOf: map[string]*core.Case{}, EmpOf: map[string]core.Employee{}}
	vm.Approvals = s.app.ApprovalsFor(u.ID)
	vm.Tasks = s.app.TasksFor(u.Role, u.ID)
	for _, ap := range vm.Approvals {
		if c, ok := s.app.Case(ap.CaseID); ok {
			vm.CaseOf[ap.CaseID] = c
			if e, ok := s.app.Employee(c.EmployeeID); ok {
				vm.EmpOf[ap.CaseID] = e
			}
		}
	}
	s.render(w, r, "work", "work", "Your work", vm)
}

type approvalVM struct {
	A        *core.Approval
	Case     *core.Case
	Emp      core.Employee
	Args     []kv
	Hash     string
	Editable bool
	Mandate  *core.Mandate
	Reserved kernel.ReservedDef
}

type kv struct{ K, V string }

func (s *Server) approvalPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	ap, ok := s.app.Approval(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if ap.ApproverID != u.ID && u.Role != "auditor" && u.Role != "hrops" {
		http.Error(w, "403: this approval belongs to its named approver", http.StatusForbidden)
		return
	}
	c, _ := s.app.Case(ap.CaseID)
	emp := core.Employee{}
	if c != nil {
		emp, _ = s.app.Employee(c.EmployeeID)
	}
	rdef, _ := s.app.ReservedByID(ap.ReservedID)
	vm := approvalVM{A: ap, Case: c, Emp: emp, Hash: core.MustHashCanon(ap.Request),
		Editable: ap.Status == core.ApprovalOpen && ap.ReservedID == "RSV-1" && ap.ApproverID == u.ID,
		Reserved: rdef}
	if args, ok := ap.Request["args"].(map[string]any); ok {
		keys := make([]string, 0, len(args))
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			vm.Args = append(vm.Args, kv{K: strings.ReplaceAll(k, "_", " "), V: fmt.Sprint(args[k])})
		}
	}
	s.render(w, r, "approval", "work", "Approval "+ap.ID, vm)
}

func (s *Server) approvalDecide(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	edits := map[string]any{}
	if v := r.FormValue("last_working_day"); v != "" {
		edits["last_working_day"] = v
	}
	if r.FormValue("edit_buyout") == "1" {
		edits["notice_buyout"] = r.FormValue("notice_buyout")
	}
	err := s.app.DecideApproval(id, u.ID, r.FormValue("pin"),
		r.FormValue("decision") == "approve", edits, r.FormValue("reason"))
	if err != nil {
		s.setFlash(r, err.Error())
	} else if r.FormValue("decision") == "approve" {
		s.setFlash(r, "Approved. The mandate binds exactly what you saw; the dispatcher re-checks the hash before anything leaves.")
	} else {
		s.setFlash(r, "Rejected and receipted.")
	}
	http.Redirect(w, r, "/approvals/"+id, http.StatusSeeOther)
}

// safeBack keeps redirects on this site: a same-site relative path only —
// parsed, no scheme, no host, a single leading slash, and no backslash or
// control character anywhere.
func safeBack(v string) string {
	for _, r := range v {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "/work"
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/work"
	}
	return v
}

func (s *Server) taskDone(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.app.CompleteTask(r.PathValue("id"), u.ID, r.FormValue("note")); err != nil {
		s.setFlash(r, err.Error())
	}
	http.Redirect(w, r, safeBack(r.FormValue("back")), http.StatusSeeOther)
}

func (s *Server) taskSkip(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.app.SkipTask(r.PathValue("id"), u.ID); err != nil {
		s.setFlash(r, err.Error())
	}
	http.Redirect(w, r, safeBack(r.FormValue("back")), http.StatusSeeOther)
}

// ---------------------------------------------------------------- cases

type stepVM struct {
	N        int
	ID       string
	Title    string
	Kind     core.StepKind
	Reserved string
	Status   core.StepStatus
	Actor    string
	Evidence string
	Note     string
	Human    bool
	Checked  bool
}

type clockVM struct {
	O         *core.Obligation
	Trace     string
	DaysLeft  int
}

type caseVM struct {
	C         *core.Case
	Emp       core.Employee
	Steps     []stepVM
	Clocks    []clockVM
	InDoubt   []*core.InDoubtItem
	Receipts  []core.Receipt
	Auto      string // "n of m"
	AutoPct   string
	Coverage  map[string]int
	FF        *kernel.FFFacts
}

func (s *Server) buildCaseVM(c *core.Case, forLeaver bool) caseVM {
	vm := caseVM{C: c, Coverage: map[string]int{}}
	vm.Emp, _ = s.app.Employee(c.EmployeeID)
	defs := s.app.StepDefs(c)
	doneAuto, doneAll := 0, 0
	for _, d := range defs {
		st := c.Steps[d.ID]
		if st == nil {
			st = &core.StepState{ID: d.ID, Status: core.StepPending}
		}
		sv := stepVM{N: d.N, ID: d.ID, Title: d.Title, Kind: d.Kind, Reserved: d.Reserved,
			Status: st.Status, Actor: st.Actor, Evidence: st.Evidence, Note: st.Note,
			Human: d.Kind == core.StepApproval || d.Kind == core.StepHumanInput,
			Checked: st.Evidence == core.EvAttributed || st.Evidence == core.EvObserved}
		if u, ok := s.app.User(st.Actor); ok {
			sv.Actor = u.Name
		}
		vm.Steps = append(vm.Steps, sv)
		if st.Status == core.StepDone || st.Status == core.StepSkipped {
			doneAll++
			if !st.HumanTouch {
				doneAuto++
			}
		}
		if st.Evidence != "" && d.Effect != nil && d.Effect.Target != "internal" {
			vm.Coverage[st.Evidence]++
		}
	}
	vm.Auto = fmt.Sprintf("%d of %d", doneAuto, len(defs))
	if doneAll > 0 {
		vm.AutoPct = fmt.Sprintf("%.0f%%", 100*float64(doneAuto)/float64(len(defs)))
	}
	for _, ob := range s.app.ObligationsFor(c.ID) {
		cv := clockVM{O: ob}
		var tr []string
		tr = append(tr, fmt.Sprintf("start %s (%s)", kernel.FDisplay(ob.StartDate), ob.StartEvent))
		for _, sk := range ob.SkippedDays {
			tr = append(tr, "skip "+sk)
		}
		tr = append(tr, "due "+kernel.FDisplay(ob.DueDate))
		cv.Trace = strings.Join(tr, " → ")
		if d, err := time.Parse("2006-01-02", ob.DueDate); err == nil {
			cv.DaysLeft = int(time.Until(d).Hours()/24) + 1
		}
		vm.Clocks = append(vm.Clocks, cv)
	}
	for _, it := range s.app.InDoubt(false) {
		if it.CaseID == c.ID {
			vm.InDoubt = append(vm.InDoubt, it)
		}
	}
	limit := 400
	if forLeaver {
		limit = 0
	}
	all := s.app.Receipts(limit, c.ID)
	if forLeaver {
		// The passbook: receipts the leaver can read, no internals.
		keep := map[string]bool{"case.opened": true, "step.status": false,
			"separation.accepted": true, "obligation.started": true, "obligation.closed": true,
			"payment.released": true, "document.issued": true, "message.sent": true,
			"case.closed": true, "effect.readback": true}
		for _, rr := range all {
			if keep[rr.Action] {
				vm.Receipts = append(vm.Receipts, rr)
			}
		}
	} else {
		vm.Receipts = all
	}
	if ff, ok := c.Facts["ff"]; ok {
		if f, ok2 := kernel.AsFF(ff); ok2 {
			vm.FF = &f
		}
	}
	return vm
}

func (s *Server) casesPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "hradmin", "auditor", "manager", "payroll"); !ok {
		return
	}
	type row struct {
		C    *core.Case
		Emp  core.Employee
		Auto string
		Open int
	}
	var rows []row
	for _, c := range s.app.Cases() {
		vm := s.buildCaseVM(c, false)
		open := 0
		for _, cl := range vm.Clocks {
			if cl.O.Status != core.ClockClosed {
				open++
			}
		}
		rows = append(rows, row{C: c, Emp: vm.Emp, Auto: vm.Auto, Open: open})
	}
	s.render(w, r, "cases", "cases", "Cases", rows)
}

func (s *Server) casePage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "hradmin", "auditor", "manager", "payroll"); !ok {
		return
	}
	c, ok := s.app.Case(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	vm := s.buildCaseVM(c, false)
	s.render(w, r, "case", "cases", c.ID, vm)
}

// ---------------------------------------------------------------- in doubt

func (s *Server) indoubtPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "payroll", "hradmin", "auditor"); !ok {
		return
	}
	type vm struct {
		Open   []*core.InDoubtItem
		Closed []*core.InDoubtItem
	}
	var v vm
	for _, it := range s.app.InDoubt(false) {
		if it.Open {
			v.Open = append(v.Open, it)
		} else {
			v.Closed = append(v.Closed, it)
		}
	}
	s.render(w, r, "indoubt", "indoubt", "In doubt", v)
}

func (s *Server) indoubtResolve(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "payroll")
	if !ok {
		return
	}
	err := s.app.ResolveInDoubt(r.PathValue("id"), u.ID, r.FormValue("resolution"), r.FormValue("note"))
	if err != nil {
		s.setFlash(r, err.Error())
	} else {
		s.setFlash(r, "Resolved and receipted.")
	}
	http.Redirect(w, r, "/indoubt", http.StatusSeeOther)
}

// ---------------------------------------------------------------- dashboard

type dashVM struct {
	CasesOpen, CasesClosed int
	Auto                   string
	AutoNote               string
	Coverage               map[string]int
	CoverageNote           string
	ClocksOpen, ClocksAtRisk, ClocksClosed, ClocksBreached, ClocksOnTime int
	InDoubtOpen            int
	Seq                    int64
	Head                   string
	Cp                     core.Checkpoint
	SignerErr              string
	ModelCalls             int
	ModelTokens            int64
	ModelName              string
	Canaries               int
	CanariesHeld           int
}

func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	v := dashVM{Coverage: map[string]int{}}
	autoDone, autoAll := 0, 0
	for _, c := range s.app.Cases() {
		if c.Status == core.CaseOpen {
			v.CasesOpen++
		} else {
			v.CasesClosed++
		}
		vm := s.buildCaseVM(c, false)
		for _, st := range vm.Steps {
			if st.Status == core.StepDone || st.Status == core.StepSkipped {
				autoAll++
				if !(st.Human && st.Status == core.StepDone) {
					// counted precisely below from receipts' human_touch
				}
			}
		}
		for k, n := range vm.Coverage {
			v.Coverage[k] += n
		}
		for _, cl := range vm.Clocks {
			switch cl.O.Status {
			case core.ClockOpen:
				v.ClocksOpen++
			case core.ClockAtRisk:
				v.ClocksAtRisk++
			case core.ClockBreached:
				v.ClocksBreached++
			case core.ClockClosed:
				v.ClocksClosed++
				if !cl.O.ClosedAt.IsZero() && cl.O.ClosedAt.Format("2006-01-02") <= cl.O.DueDate {
					v.ClocksOnTime++
				}
			}
		}
	}
	// Automation measured from receipts: steps done with no human touch.
	humanDone, allDone := 0, 0
	for _, rr := range s.app.Receipts(0, "") {
		if rr.Action == "step.status" {
			if st, _ := rr.Data["status"].(string); st == "done" || st == "skipped" {
				allDone++
				if ht, _ := rr.Data["human_touch"].(bool); ht {
					humanDone++
				}
			}
		}
	}
	_ = autoDone
	_ = autoAll
	if allDone > 0 {
		v.Auto = fmt.Sprintf("%d of %d", allDone-humanDone, allDone)
		v.AutoNote = fmt.Sprintf("measured on this instance's receipts (design target: 15 of 19 per exit); %d completions, %d by a person", allDone, humanDone)
	} else {
		v.Auto = "no steps completed yet"
	}
	v.CoverageNote = "external effects by evidence level; reply-only means the target offered nothing to read back"
	v.InDoubtOpen = len(s.app.InDoubt(true))
	v.Seq, v.Head = s.app.Chain().Head()
	v.Cp, v.SignerErr = s.app.Chain().LastCheckpoint()
	v.ModelCalls, v.ModelTokens = s.app.ModelSpend()
	v.ModelName = s.app.Router().ModelName()
	for _, c := range s.app.CanaryResults() {
		v.Canaries++
		if held, _ := c["held"].(bool); held {
			v.CanariesHeld++
		}
	}
	s.render(w, r, "dashboard", "dashboard", "Dashboard", v)
}

// ---------------------------------------------------------------- admin

type adminVM struct {
	Kill     kernel.KillSwitch
	Users    []core.User
	Packages []*kernel.Package
	Checks   map[string]kernel.CheckResult
	Canaries []kernel.CanaryResult
	Reserved []kernel.ReservedDef
	MocksURL string
	TamperOn bool
}

var lastChecks = map[string]kernel.CheckResult{}
var lastCanaries []kernel.CanaryResult

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "hradmin"); !ok {
		return
	}
	v := adminVM{Kill: s.app.Kill(), Users: s.app.Users(), Packages: s.app.Packages(),
		Checks: lastChecks, Canaries: lastCanaries, Reserved: s.app.ReservedList(),
		MocksURL: s.app.Config().MocksURL, TamperOn: s.app.TamperNext()}
	s.render(w, r, "admin", "admin", "Controls", v)
}

func (s *Server) adminKill(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "hradmin")
	if !ok {
		return
	}
	on := r.FormValue("on") == "1"
	_ = s.app.SetKillSwitch(u.ID, on, r.FormValue("scope"))
	if on {
		s.setFlash(r, "Kill switch ON: the next action is blocked. Clocks keep running — pausing agents never extends a legal deadline.")
	} else {
		s.setFlash(r, "Kill switch off; the engine resumes from the receipts.")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminLie(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "hradmin"); !ok {
		return
	}
	svc, on := r.FormValue("svc"), r.FormValue("on")
	req, _ := http.NewRequest("POST", s.app.Config().MocksURL+"/admin/"+svc+"/lie?on="+on, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		s.setFlash(r, fmt.Sprintf("%s lie mode set to %s on the stand-in.", svc, on))
	} else {
		s.setFlash(r, "could not reach the stand-ins: "+err.Error())
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminTamper(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "hrops", "hradmin"); !ok {
		return
	}
	s.app.SetTamperNext(r.FormValue("on") == "1")
	s.setFlash(r, "The next reserved dispatch will have ₹1 added after approval — the gate must refuse it.")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminCanaries(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "hradmin")
	if !ok {
		return
	}
	lastCanaries = s.app.RunCanaries(u.ID)
	held := 0
	for _, c := range lastCanaries {
		if c.Held {
			held++
		}
	}
	s.setFlash(r, fmt.Sprintf("Canaries: %d of %d held, each receipted.", held, len(lastCanaries)))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminPkgCheck(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hrops", "hradmin")
	if !ok {
		return
	}
	name := r.FormValue("pkg")
	res, err := s.app.RunCheck(name, u.ID)
	if err != nil {
		s.setFlash(r, err.Error())
	} else {
		lastChecks[name] = res
		if res.Errors == 0 {
			s.setFlash(r, fmt.Sprintf("cfoil check: PASS — 0 errors, %d warning(s). Certificate %s… (inactive until two named people activate it).", res.Warnings, core.Short(res.Cert)))
		} else {
			s.setFlash(r, fmt.Sprintf("cfoil check: REFUSED — %d error(s). Nothing is loaded.", res.Errors))
		}
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminPkgActivate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hradmin", "hrops")
	if !ok {
		return
	}
	name := r.FormValue("pkg")
	confirmer := r.FormValue("confirmer")
	cu, okc := s.app.User(confirmer)
	if !okc || cu.PIN != r.FormValue("confirmer_pin") {
		s.setFlash(r, "activation needs the second person's PIN")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if u2, _ := s.app.User(u.ID); u2.PIN != r.FormValue("pin") {
		s.setFlash(r, "re-authentication failed: wrong PIN")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if err := s.app.Activate(name, u.ID, confirmer); err != nil {
		s.setFlash(r, err.Error())
	} else {
		s.setFlash(r, name+" activated by two named people; receipted with its certificate. Zero core files changed.")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminReservedAdd(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r, "hradmin")
	if !ok {
		return
	}
	def := kernel.ReservedDef{ID: strings.TrimSpace(r.FormValue("id")),
		Action: strings.TrimSpace(r.FormValue("action")),
		Title:  strings.TrimSpace(r.FormValue("title")),
		ApproverRole: r.FormValue("approver_role")}
	if err := s.app.AddReserved(u.ID, def); err != nil {
		s.setFlash(r, err.Error())
	} else {
		s.setFlash(r, def.ID+" added to the reserved list (the floor itself can never be removed).")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---------------------------------------------------------------- audit

type auditVM struct {
	Seq       int64
	Head      string
	Cp        core.Checkpoint
	SignerErr string
	Receipts  []core.Receipt
	ExportDir string
	Exported  bool
	Checks    []audit.Check
	CheckedAt time.Time
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "auditor", "hradmin", "hrops"); !ok {
		return
	}
	v := auditVM{Receipts: s.app.Receipts(250, r.URL.Query().Get("case")),
		ExportDir: s.exportDir}
	v.Seq, v.Head = s.app.Chain().Head()
	v.Cp, v.SignerErr = s.app.Chain().LastCheckpoint()
	if _, err := os.Stat(filepath.Join(s.exportDir, "receipts.jsonl")); err == nil {
		v.Exported = true
	}
	s.mu.Lock()
	v.Checks, v.CheckedAt = s.lastAudit, s.lastAuditAt
	s.mu.Unlock()
	s.render(w, r, "audit", "audit", "Audit", v)
}

func (s *Server) auditExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "auditor", "hradmin", "hrops"); !ok {
		return
	}
	s.app.Chain().RequestCheckpoint()
	if err := s.doExport(); err != nil {
		s.setFlash(r, "export failed: "+err.Error())
	} else {
		s.setFlash(r, "Bundle exported to "+s.exportDir+" — verify it offline with: go run ./cmd/verify-audit -dir export")
	}
	http.Redirect(w, r, "/audit", http.StatusSeeOther)
}

func (s *Server) doExport() error {
	if err := os.MkdirAll(s.exportDir, 0o755); err != nil {
		return err
	}
	rPath, cpPath := s.app.Chain().Paths()
	for _, pair := range [][2]string{
		{rPath, filepath.Join(s.exportDir, "receipts.jsonl")},
		{cpPath, filepath.Join(s.exportDir, "checkpoints.jsonl")},
	} {
		b, err := os.ReadFile(pair[0])
		if err != nil {
			return err
		}
		if err := os.WriteFile(pair[1], b, 0o644); err != nil {
			return err
		}
	}
	// The signer's public key is written beside the bundle from the live
	// signer (a channel outside the bundle), so the verifier can pin it.
	if pub := s.fetchSignerPub(); pub != "" {
		if err := os.WriteFile(filepath.Join(s.exportDir, "signer_pub.txt"), []byte(pub+"\n"), 0o644); err != nil {
			return err
		}
	}
	readme := "Counterfoil audit bundle.\n" +
		"Verify offline (no network, no database):\n  go run ./cmd/verify-audit -dir export\n" +
		"signer_pub.txt pins the signer's public key (fetched from the signer, not from the bundle);\n" +
		"hand it to the auditor separately, or pass it with -pub. Receipts hold hashes, not personal text.\n"
	return os.WriteFile(filepath.Join(s.exportDir, "README.txt"), []byte(readme), 0o644)
}

func (s *Server) fetchSignerPub() string {
	resp, err := http.Get(s.app.Config().SignerURL + "/pub")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != 200 || len(b) < 32 {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) auditVerify(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, "auditor", "hradmin", "hrops"); !ok {
		return
	}
	if err := s.doExport(); err != nil {
		s.setFlash(r, "export failed: "+err.Error())
		http.Redirect(w, r, "/audit", http.StatusSeeOther)
		return
	}
	livePub := s.fetchSignerPub()
	checks, err := audit.Run(s.exportDir, livePub)
	if err != nil {
		s.setFlash(r, "verify failed: "+err.Error())
	} else {
		pass := 0
		for _, c := range checks {
			if c.Pass {
				pass++
			}
		}
		pin := "signer key pinned from the live signer"
		if livePub == "" {
			pin = "live signer unreachable, so signatures were checked UNPINNED"
		}
		s.setFlash(r, fmt.Sprintf("Offline verifier: %d of %d checks pass on the exported bundle (%s; same code as cmd/verify-audit).", pass, len(checks), pin))
	}
	s.mu.Lock()
	s.lastAudit, s.lastAuditAt = checks, time.Now()
	s.mu.Unlock()
	http.Redirect(w, r, "/audit", http.StatusSeeOther)
}

// ---------------------------------------------------------------- letter

func (s *Server) letterPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.user(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	c, ok := s.app.Case(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The letter belongs to its leaver; beyond them, only staff roles.
	staff := map[string]bool{"hrops": true, "hradmin": true, "auditor": true}
	if u.EmpID != c.EmployeeID && !staff[u.Role] {
		http.Error(w, "403: this letter belongs to the employee it names", http.StatusForbidden)
		return
	}
	emp, _ := s.app.Employee(c.EmployeeID)
	var seq int64
	var hash string
	for _, rr := range s.app.Receipts(0, c.ID) {
		if rr.Action == "document.issued" {
			seq, hash = rr.Seq, rr.Hash
			break
		}
	}
	type vm struct {
		C    *core.Case
		Emp  core.Employee
		Seq  int64
		Hash string
	}
	s.render(w, r, "letter", "", "Relieving letter", vm{C: c, Emp: emp, Seq: seq, Hash: hash})
}
