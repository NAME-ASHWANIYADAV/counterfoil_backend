package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"counterfoil/internal/core"
)

// Config wires one cfoil instance.
type Config struct {
	DataDir         string
	MocksURL        string // base URL of the target stand-ins
	SignerURL       string
	ExtDir          string // extensions/
	PolicyPath      string
	RulesPath       string
	UsersPath       string
	EmployeesPath   string
	CheckpointEvery int64
	DispatchTimeout time.Duration
	SettleWait      time.Duration // out-of-model grace before in-doubt
}

// App is the kernel: all state is rebuilt by replaying the receipt chain,
// so the receipts are not beside the system of record — they are it.
type App struct {
	cfg    Config
	chain  *core.Chain
	ledger *core.Ledger
	rules  *RulePack

	mu          sync.Mutex
	users       map[string]core.User
	employees   map[string]core.Employee
	reserved    []ReservedDef
	packages    map[string]*Package
	cases       map[string]*core.Case
	approvals   map[string]*core.Approval
	mandates    map[string]*core.Mandate
	indoubt     map[string]*core.InDoubtItem
	tasks       map[string]*core.Task
	obligations map[string]*core.Obligation
	kill        KillSwitch
	canary      map[string]map[string]any
	modelCalls  int
	modelTokens int64
	tamperNext  bool // demo: tamper the next reserved dispatch by one rupee
	counters    map[string]int

	chat    *core.Log // personal text lives here, NOT in receipts
	svy     *core.Log // survey answers: same rule — the receipt carries a hash
	chatMu  sync.Mutex
	chatLog map[string][]ChatMsg // case id -> transcript (memory)

	router *Router
}

type ChatMsg struct {
	CaseID string    `json:"case_id"`
	From   string    `json:"from"` // user id or "counterfoil"
	Text   string    `json:"text"`
	Kind   string    `json:"kind,omitempty"` // "" | card | system
	TS     time.Time `json:"ts"`
}

func New(cfg Config) (*App, error) {
	if cfg.CheckpointEvery == 0 {
		cfg.CheckpointEvery = 10
	}
	if cfg.DispatchTimeout == 0 {
		cfg.DispatchTimeout = 1500 * time.Millisecond
	}
	if cfg.SettleWait == 0 {
		cfg.SettleWait = 3 * time.Second
	}
	chain, err := core.OpenChain(cfg.DataDir, cfg.SignerURL, cfg.CheckpointEvery)
	if err != nil {
		return nil, err
	}
	led, err := core.OpenLedger(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	chat, err := core.OpenLog(filepath.Join(cfg.DataDir, "chat.jsonl"))
	if err != nil {
		return nil, err
	}
	svy, err := core.OpenLog(filepath.Join(cfg.DataDir, "surveys.jsonl"))
	if err != nil {
		return nil, err
	}
	a := &App{
		cfg: cfg, chain: chain, ledger: led, chat: chat, svy: svy,
		users: map[string]core.User{}, employees: map[string]core.Employee{},
		packages: map[string]*Package{}, cases: map[string]*core.Case{},
		approvals: map[string]*core.Approval{}, mandates: map[string]*core.Mandate{},
		indoubt: map[string]*core.InDoubtItem{}, tasks: map[string]*core.Task{},
		obligations: map[string]*core.Obligation{}, canary: map[string]map[string]any{},
		counters: map[string]int{}, chatLog: map[string][]ChatMsg{},
	}
	a.router = NewRouter(a)

	if err := a.loadStatics(); err != nil {
		return nil, err
	}
	// Replay the chain to rebuild state.
	rs, err := core.ReadAll[core.Receipt](filepath.Join(cfg.DataDir, "receipts.jsonl"))
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	for _, r := range rs {
		a.apply(r)
	}
	a.mu.Unlock()
	// Replay chat transcripts (demo convenience; personal text, not receipts).
	msgs, _ := core.ReadAll[ChatMsg](chat.Path())
	for _, m := range msgs {
		a.chatLog[m.CaseID] = append(a.chatLog[m.CaseID], m)
	}
	// First boot: receipt the floor and the built-in package activation.
	if len(rs) == 0 {
		if err := a.bootstrap(); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *App) loadStatics() error {
	read := func(path string, v any) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	var users []core.User
	if err := read(a.cfg.UsersPath, &users); err != nil {
		return fmt.Errorf("users: %w", err)
	}
	for _, u := range users {
		a.users[u.ID] = u
	}
	var emps []core.Employee
	if err := read(a.cfg.EmployeesPath, &emps); err != nil {
		return fmt.Errorf("employees: %w", err)
	}
	for _, e := range emps {
		a.employees[e.ID] = e
	}
	rsv, err := LoadReserved(a.cfg.PolicyPath)
	if err != nil {
		return fmt.Errorf("reserved: %w", err)
	}
	a.reserved = rsv
	rp, err := LoadRules(a.cfg.RulesPath)
	if err != nil {
		return fmt.Errorf("rules: %w", err)
	}
	a.rules = rp
	dirs, err := os.ReadDir(a.cfg.ExtDir)
	if err != nil {
		return fmt.Errorf("extensions: %w", err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		m, err := LoadManifest(filepath.Join(a.cfg.ExtDir, d.Name()))
		if err != nil {
			return fmt.Errorf("package %s: %w", d.Name(), err)
		}
		a.packages[m.Name] = &Package{Manifest: *m, Dir: filepath.Join(a.cfg.ExtDir, d.Name())}
	}
	return nil
}

func (a *App) bootstrap() error {
	if err := a.emit(core.Receipt{
		Action: "platform.initialised", ActorType: "system",
		Data: map[string]any{
			"product": "Counterfoil", "note": "reserved floor loaded from policy/reserved.json",
			"floor_count": len(a.reserved)},
	}); err != nil {
		return err
	}
	// The exits package ships pre-activated so the demo opens on a working
	// journey; the activation is still receipted, with its certificate.
	if p, ok := a.packages["exits"]; ok {
		res := CheckManifest(&p.Manifest, func(action string) (ReservedDef, bool) { return a.Reserved(action) })
		if res.Errors > 0 {
			return fmt.Errorf("built-in exits package fails its own admission check: %+v", res.Findings)
		}
		if err := a.emit(core.Receipt{
			Action: "package.checked", ActorType: "system", ActorID: "bootstrap",
			Data: map[string]any{"package": "exits", "errors": 0, "warnings": res.Warnings,
				"certificate": res.Cert, "stages": "schema + lint only (crash test, Cedar and SymCC stages: design, not built)"},
		}); err != nil {
			return err
		}
		if err := a.emit(core.Receipt{
			Action: "package.activated", ActorType: "system", ActorID: "bootstrap",
			Data: map[string]any{"package": "exits", "certificate": res.Cert,
				"proposed_by": "bootstrap", "confirmed_by": "bootstrap",
				"note": "built-in demo package; team activation shown live with the onboarding package"},
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- emit / apply

// emit appends a receipt to the chain and applies it to state. Callers
// must NOT hold a.mu.
func (a *App) emit(r core.Receipt) error {
	done, err := a.chain.Append(r)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.apply(done)
	a.mu.Unlock()
	a.tryCloseClocks(done)
	return nil
}

func asJSON[T any](v any) (T, bool) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, false
	}
	return out, true
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// apply folds one receipt into state. It must be deterministic: replaying
// the chain from zero rebuilds exactly this state. Called with a.mu held.
func (a *App) apply(r core.Receipt) {
	d := r.Data
	switch r.Action {
	case "employee.created":
		if e, ok := asJSON[core.Employee](d["employee"]); ok {
			a.employees[e.ID] = e
			a.noteID("employee", e.ID)
		}
	case "case.opened":
		c, ok := asJSON[core.Case](d["case"])
		if !ok {
			return
		}
		cc := c
		if cc.Steps == nil {
			cc.Steps = map[string]*core.StepState{}
		}
		a.cases[cc.ID] = &cc
		a.noteID("case", cc.ID)
	case "case.updated":
		if c := a.cases[r.CaseID]; c != nil {
			if facts, ok := d["facts"].(map[string]any); ok {
				if c.Facts == nil {
					c.Facts = map[string]any{}
				}
				for k, v := range facts {
					c.Facts[k] = v
				}
			}
			if v := str(d, "bound_lwd"); v != "" {
				c.BoundLWD = v
			}
			if v, ok := d["buyout"].(bool); ok {
				c.Buyout = v
			}
			if v, ok := d["sealed"].(bool); ok {
				c.Sealed = v
			}
		}
	case "case.closed":
		if c := a.cases[r.CaseID]; c != nil {
			c.Status = core.CaseClosed
			c.ClosedAt = r.TS
		}
	case "step.status":
		if c := a.cases[r.CaseID]; c != nil {
			st := c.Steps[r.StepID]
			if st == nil {
				st = &core.StepState{ID: r.StepID}
				c.Steps[r.StepID] = st
			}
			st.Status = core.StepStatus(str(d, "status"))
			if v := str(d, "actor"); v != "" {
				st.Actor = v
			}
			if v, ok := d["human_touch"].(bool); ok {
				st.HumanTouch = v
			}
			if v := r.Evidence; v != "" {
				st.Evidence = v
			}
			if v := str(d, "note"); v != "" {
				st.Note = v
			}
			if st.Status == core.StepDone {
				st.DoneAt = r.TS
			}
		}
	case "approval.created":
		ap, ok := asJSON[core.Approval](d["approval"])
		if ok {
			cp := ap
			a.approvals[cp.ID] = &cp
			a.noteID("approval", cp.ID)
		}
	case "approval.decided":
		if ap := a.approvals[str(d, "approval_id")]; ap != nil {
			ap.Status = core.ApprovalStatus(str(d, "status"))
			ap.Decided = r.TS
			ap.Reason = str(d, "reason")
			if req, ok := d["request"].(map[string]any); ok {
				ap.Request = req
			}
		}
	case "mandate.issued":
		m, ok := asJSON[core.Mandate](d["mandate"])
		if ok {
			cp := m
			a.mandates[cp.ID] = &cp
			a.noteID("mandate", cp.ID)
		}
	case "mandate.used":
		if m := a.mandates[str(d, "mandate_id")]; m != nil {
			m.UsedAt = r.TS
		}
	case "indoubt.opened":
		it, ok := asJSON[core.InDoubtItem](d["item"])
		if ok {
			cp := it
			a.indoubt[cp.ID] = &cp
			a.noteID("indoubt", cp.ID)
		}
	case "indoubt.resolved":
		if it := a.indoubt[str(d, "item_id")]; it != nil {
			it.Open = false
			it.Resolved = r.TS
			it.Resolution = str(d, "resolution")
			it.ResolvedBy = r.ActorID
		}
	case "task.created":
		t, ok := asJSON[core.Task](d["task"])
		if ok {
			cp := t
			a.tasks[cp.ID] = &cp
			a.noteID("task", cp.ID)
		}
	case "task.done":
		if t := a.tasks[str(d, "task_id")]; t != nil {
			t.Open = false
			t.DoneBy = r.ActorID
			t.DoneAt = r.TS
		}
	case "obligation.started":
		ob, ok := asJSON[core.Obligation](d["obligation"])
		if ok {
			cp := ob
			a.obligations[cp.ID] = &cp
		}
	case "obligation.status":
		if ob := a.obligations[str(d, "obligation_id")]; ob != nil {
			ob.Status = core.ClockStatus(str(d, "status"))
		}
	case "obligation.closed":
		if ob := a.obligations[str(d, "obligation_id")]; ob != nil {
			ob.Status = core.ClockClosed
			if seq, ok := d["closed_by_receipt"].(float64); ok {
				ob.ClosedBy = int64(seq)
			}
			ob.ClosedAt = r.TS
		}
	case "killswitch.set":
		on, _ := d["on"].(bool)
		a.kill = KillSwitch{On: on, Scope: str(d, "scope"), By: r.ActorID, At: r.TS}
	case "reserved.added":
		a.reserved = append(a.reserved, ReservedDef{
			ID: str(d, "id"), Action: str(d, "action"), Title: str(d, "title"),
			ApproverRole: str(d, "approver_role"), Floor: false,
		})
	case "package.activated":
		if p := a.packages[str(d, "package")]; p != nil {
			p.Active = true
			p.Cert = str(d, "certificate")
			p.ActivatedBy = []string{str(d, "proposed_by"), str(d, "confirmed_by")}
		}
	case "package.checked":
		if p := a.packages[str(d, "package")]; p != nil {
			p.Cert = str(d, "certificate")
		}
	case "canary.run":
		id := str(d, "id")
		a.canary[id] = d
	case "model.call":
		a.modelCalls++
		if t, ok := d["tokens"].(float64); ok {
			a.modelTokens += int64(t)
		}
	}
}

// next returns a replay-stable sequence number for an id class.
func (a *App) next(kind string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.counters[kind]++
	return a.counters[kind]
}

// noteID re-seeds a counter from an id seen during replay, so a restart
// can never mint an id the chain already holds. Called from apply only
// (the caller holds the lock). The numeric tail of "XX-0042" is 42.
func (a *App) noteID(kind, id string) {
	i := strings.LastIndexByte(id, '-')
	if i < 0 {
		return
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil {
		return
	}
	if kind == "employee" {
		// Employee records minted by onboarding are numbered 2000+n.
		if n < 2000 {
			return
		}
		n -= 2000
	}
	if n > a.counters[kind] {
		a.counters[kind] = n
	}
}

// ---------------------------------------------------------------- queries

func (a *App) User(id string) (core.User, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	return u, ok
}

func (a *App) Users() []core.User {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]core.User, 0, len(a.users))
	for _, u := range a.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *App) Employee(id string) (core.Employee, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.employees[id]
	return e, ok
}

func (a *App) Case(id string) (*core.Case, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.cases[id]
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

func (a *App) Cases() []*core.Case {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*core.Case, 0, len(a.cases))
	for _, c := range a.cases {
		if strings.HasPrefix(c.ID, "CANARY") {
			continue
		}
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.After(out[j].OpenedAt) })
	return out
}

func (a *App) CaseForEmployee(empID string) (*core.Case, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.cases {
		if c.EmployeeID == empID && c.Status == core.CaseOpen {
			cp := *c
			return &cp, true
		}
	}
	for _, c := range a.cases {
		if c.EmployeeID == empID {
			cp := *c
			return &cp, true
		}
	}
	return nil, false
}

func (a *App) ApprovalsFor(userID string) []*core.Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*core.Approval
	for _, ap := range a.approvals {
		if ap.ApproverID == userID {
			cp := *ap
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (a *App) Approval(id string) (*core.Approval, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap, ok := a.approvals[id]
	if !ok {
		return nil, false
	}
	cp := *ap
	return &cp, true
}

func (a *App) InDoubt(openOnly bool) []*core.InDoubtItem {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*core.InDoubtItem
	for _, it := range a.indoubt {
		if openOnly && !it.Open {
			continue
		}
		cp := *it
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (a *App) TasksFor(role, userID string) []*core.Task {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*core.Task
	for _, t := range a.tasks {
		if !t.Open {
			continue
		}
		if (t.UserID != "" && t.UserID == userID) || (t.UserID == "" && t.Role == role) {
			cp := *t
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (a *App) ObligationsFor(caseID string) []*core.Obligation {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*core.Obligation
	for _, ob := range a.obligations {
		if caseID == "" || ob.CaseID == caseID {
			cp := *ob
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *App) Kill() KillSwitch {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.kill
}

func (a *App) Rules() *RulePack { return a.rules }

func (a *App) Receipts(limit int, caseID string) []core.Receipt {
	rs, err := core.ReadAll[core.Receipt](filepath.Join(a.cfg.DataDir, "receipts.jsonl"))
	if err != nil {
		return nil
	}
	var out []core.Receipt
	for i := len(rs) - 1; i >= 0; i-- {
		if caseID != "" && rs[i].CaseID != caseID {
			continue
		}
		out = append(out, rs[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (a *App) Chain() *core.Chain   { return a.chain }
func (a *App) Ledger() *core.Ledger { return a.ledger }
func (a *App) Config() Config       { return a.cfg }
func (a *App) Router() *Router      { return a.router }

func (a *App) CanaryResults() map[string]map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]map[string]any{}
	for k, v := range a.canary {
		out[k] = v
	}
	return out
}

func (a *App) ModelSpend() (calls int, tokens int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.modelCalls, a.modelTokens
}

func (a *App) SetTamperNext(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tamperNext = on
}

func (a *App) TamperNext() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tamperNext
}

// ---------------------------------------------------------------- chat

func (a *App) AddChat(m ChatMsg) {
	m.TS = time.Now().UTC()
	a.chatMu.Lock()
	a.chatLog[m.CaseID] = append(a.chatLog[m.CaseID], m)
	a.chatMu.Unlock()
	_ = a.chat.Append(m)
}

func (a *App) ChatFor(caseID string) []ChatMsg {
	a.chatMu.Lock()
	defer a.chatMu.Unlock()
	out := make([]ChatMsg, len(a.chatLog[caseID]))
	copy(out, a.chatLog[caseID])
	return out
}
