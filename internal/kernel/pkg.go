package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"counterfoil/internal/core"
)

// A service line ships as one package: steps, effects and read-back
// declarations as data. No compiled code enters cfoil. `cfoil check` must
// pass, and a named HR administrator plus a second person activate it —
// the core never changes (zero core diff).

type ReadBack struct {
	Strategy   string `json:"strategy"`   // attributed | observed | none
	Collection string `json:"collection"` // what to GET in the target
	Justification string `json:"justification,omitempty"` // required when strategy=none
}

type EffectDef struct {
	Action           string   `json:"action"` // e.g. directory.disable
	Target           string   `json:"target"` // internal | itsm | directory | cards | payroll
	Path             string   `json:"path,omitempty"` // POST path on the target
	ArgsFrom         []string `json:"args_from,omitempty"` // fact keys copied into args
	ReadBack         ReadBack `json:"read_back"`
	RequiredEvidence string   `json:"required_evidence"` // attributed | observed | internal
	RetrySafe        bool     `json:"retry_safe"` // target honours our key or action is convergent
	Reversibility    string   `json:"reversibility"` // reversible | convergent | compensable | irreversible
}

type StepDef struct {
	ID        string     `json:"id"`
	N         int        `json:"n"`
	Title     string     `json:"title"`
	Kind      core.StepKind `json:"kind"`
	Impl      string     `json:"impl,omitempty"`  // registered function for agent/rule steps
	Reserved  string     `json:"reserved,omitempty"` // RSV id for approvals
	ApproverRole string  `json:"approver_role,omitempty"`
	Role      string     `json:"role,omitempty"` // human_input owner role
	Auto      bool       `json:"auto"`           // runs without a person
	DependsOn []string   `json:"depends_on,omitempty"`
	At        string     `json:"at,omitempty"` // "" | "lwd" (runs at the LWD cut-off)
	Effect    *EffectDef `json:"effect,omitempty"`
	Optional  bool       `json:"optional,omitempty"` // e.g. exit survey
	// Forbidden in a safe package; the checker refuses them. Present in
	// the struct so a hostile package is *parsed* and then refused.
	ApproveIfSilent bool `json:"approve_if_silent,omitempty"`
	TimeoutApprove  int  `json:"timeout_approve_hours,omitempty"`
}

type Manifest struct {
	Format      string    `json:"format"` // counterfoil.package/v1
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	ServiceLine string    `json:"service_line"`
	Description string    `json:"description"`
	Steps       []StepDef `json:"steps"`
}

type Package struct {
	Manifest Manifest
	Dir      string
	Cert     string // canonical hash of the manifest when the check passes
	Active   bool
	ActivatedBy []string
}

func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ---------------------------------------------------------------- the admission check

type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"` // error | warning
	Where    string `json:"where"`
	Message  string `json:"message"`
}

type CheckResult struct {
	Package  string    `json:"package"`
	Findings []Finding `json:"findings"`
	Errors   int       `json:"errors"`
	Warnings int       `json:"warnings"`
	Cert     string    `json:"certificate,omitempty"` // only when 0 errors
}

// CheckManifest is the admission check (`cfoil check`): a package is
// eligible for activation only when it passes. Successor to design spike
// S2's lint stage; the generated crash test and Cedar/SymCC stages of the
// full design are not implemented here and the result says so.
func CheckManifest(m *Manifest, isReserved func(string) (ReservedDef, bool)) CheckResult {
	res := CheckResult{Package: m.Name}
	add := func(rule, sev, where, msg string) {
		res.Findings = append(res.Findings, Finding{rule, sev, where, msg})
		if sev == "error" {
			res.Errors++
		} else {
			res.Warnings++
		}
	}
	if m.Format != "counterfoil.package/v1" {
		add("E0-format", "error", "package", "format must be counterfoil.package/v1")
	}
	ids := map[string]*StepDef{}
	for i := range m.Steps {
		s := &m.Steps[i]
		w := fmt.Sprintf("step %s (%d)", s.ID, s.N)
		if s.ID == "" {
			add("E1-id", "error", w, "every step needs an id")
			continue
		}
		if _, dup := ids[s.ID]; dup {
			add("E1-id", "error", w, "duplicate step id")
		}
		ids[s.ID] = s
		switch s.Kind {
		case core.StepAgent, core.StepRule, core.StepApproval, core.StepEffect, core.StepHumanInput:
		default:
			add("E2-kind", "error", w, "unknown step kind "+string(s.Kind))
		}
		// Reserved decisions: a named human, never auto, no silence rules.
		if s.Kind == core.StepApproval {
			if s.Reserved == "" || s.ApproverRole == "" {
				add("E3-reserved", "error", w, "an approval step must name its reserved id and approver role")
			}
			if s.Auto {
				add("E3-reserved", "error", w, "an approval step can never be auto")
			}
		}
		if s.ApproveIfSilent || s.TimeoutApprove > 0 {
			add("E4-silence", "error", w, "approval by silence or timeout is forbidden: silence escalates to the next named person")
		}
		// Effects: read-back declared, evidence declared. An approval step
		// may carry the effect its mandate releases.
		if s.Kind == core.StepEffect || (s.Kind == core.StepApproval && s.Effect != nil) {
			if s.Effect == nil {
				add("E5-effect", "error", w, "an effect step must declare its effect")
			} else {
				e := s.Effect
				switch e.ReadBack.Strategy {
				case "attributed", "observed":
				case "none":
					if strings.TrimSpace(e.ReadBack.Justification) == "" {
						add("E6-readback", "error", w, "read_back none needs a written justification; the step will count against coverage")
					} else {
						add("W1-readback", "warning", w, "no read-back: the step can only ever reach reply-only evidence")
					}
				default:
					add("E6-readback", "error", w, "every effect declares read_back.strategy (attributed, observed, or none with justification)")
				}
				if e.RequiredEvidence == "" {
					add("E7-evidence", "error", w, "every effect declares required_evidence")
				}
				if rdef, ok := isReserved(e.Action); ok {
					dep := s.Kind == core.StepApproval && s.Reserved == rdef.ID
					for _, d := range s.DependsOn {
						if ds, found := ids[d]; found && ds.Kind == core.StepApproval && ds.Reserved == rdef.ID {
							dep = true
						}
					}
					// Approval may also be declared later in file order.
					for j := range m.Steps {
						other := &m.Steps[j]
						if other.Kind == core.StepApproval && other.Reserved == rdef.ID {
							for _, d := range s.DependsOn {
								if d == other.ID {
									dep = true
								}
							}
						}
					}
					if !dep {
						add("E8-mandate", "error", w, fmt.Sprintf("action %s is reserved (%s): the effect must depend on its approval step", e.Action, rdef.ID))
					}
				}
			}
		}
		if s.Title == "" {
			add("W2-title", "warning", w, "step has no title")
		}
	}
	// Dependency references and cycles.
	for _, s := range m.Steps {
		for _, d := range s.DependsOn {
			if _, ok := ids[d]; !ok {
				add("E9-deps", "error", "step "+s.ID, "depends on unknown step "+d)
			}
		}
	}
	if cycle := findCycle(m.Steps); cycle != "" {
		add("E9-deps", "error", "package", "dependency cycle: "+cycle)
	}
	sort.SliceStable(res.Findings, func(i, j int) bool { return res.Findings[i].Severity < res.Findings[j].Severity })
	if res.Errors == 0 {
		res.Cert = core.MustHashCanon(m)
	}
	return res
}

func findCycle(steps []StepDef) string {
	adj := map[string][]string{}
	for _, s := range steps {
		adj[s.ID] = s.DependsOn
	}
	state := map[string]int{}
	var stack []string
	var dfs func(string) string
	dfs = func(id string) string {
		state[id] = 1
		stack = append(stack, id)
		for _, d := range adj[id] {
			if state[d] == 1 {
				return strings.Join(append(stack, d), " -> ")
			}
			if state[d] == 0 {
				if c := dfs(d); c != "" {
					return c
				}
			}
		}
		state[id] = 2
		stack = stack[:len(stack)-1]
		return ""
	}
	for id := range adj {
		if state[id] == 0 {
			if c := dfs(id); c != "" {
				return c
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------- registry

func (a *App) Packages() []*Package {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Package, 0, len(a.packages))
	for _, p := range a.packages {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out
}

func (a *App) PackageByName(name string) (*Package, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.packages[name]
	return p, ok
}

// RunCheck runs the admission check on a loaded package and receipts the result.
func (a *App) RunCheck(name, by string) (CheckResult, error) {
	p, ok := a.PackageByName(name)
	if !ok {
		return CheckResult{}, fmt.Errorf("unknown package %s", name)
	}
	res := CheckManifest(&p.Manifest, func(action string) (ReservedDef, bool) { return a.Reserved(action) })
	err := a.emit(core.Receipt{
		Action: "package.checked", ActorType: "human", ActorID: by,
		Data: map[string]any{"package": name, "errors": res.Errors, "warnings": res.Warnings,
			"certificate": res.Cert,
			"stages": "schema + lint only (crash test, Cedar and SymCC stages: design, not built)"},
	})
	return res, err
}

// Activate needs the admission certificate and two different named people
// (proposer and confirmer) — never an agent, never one person alone.
func (a *App) Activate(name string, proposer, confirmer string) error {
	p, ok := a.PackageByName(name)
	if !ok {
		return fmt.Errorf("unknown package %s", name)
	}
	if p.Active {
		return fmt.Errorf("%s is already active", name)
	}
	res := CheckManifest(&p.Manifest, func(action string) (ReservedDef, bool) { return a.Reserved(action) })
	if res.Errors > 0 {
		return fmt.Errorf("REFUSED: %s has %d admission errors; nothing is loaded", name, res.Errors)
	}
	if proposer == "" || confirmer == "" || proposer == confirmer {
		return fmt.Errorf("activation needs two different named people")
	}
	return a.emit(core.Receipt{
		Action: "package.activated", ActorType: "human", ActorID: proposer,
		Data: map[string]any{"package": name, "certificate": res.Cert,
			"proposed_by": proposer, "confirmed_by": confirmer},
	})
}
