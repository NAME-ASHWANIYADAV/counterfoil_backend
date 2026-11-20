package core

import "time"

// ---------------------------------------------------------------- people

// User is a demo person who can sign in. The PIN stands in for the
// identity provider's re-authentication; it is demo-only and documented so.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  string `json:"role"` // leaver | manager | payroll | hrops | hradmin | auditor
	Title string `json:"title"`
	PIN   string `json:"pin"`
	EmpID string `json:"emp_id,omitempty"`
}

// Employee is a row of the synthetic HR dataset.
type Employee struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	Title     string  `json:"title"`
	ManagerID string  `json:"manager_id,omitempty"`
	Location  string  `json:"location"`
	JoinDate  string  `json:"join_date"` // YYYY-MM-DD
	MonthlyGross float64 `json:"monthly_gross_inr"`
	BasicShare   float64 `json:"basic_share"` // of gross, 0..1
	LeaveBalanceDays float64 `json:"leave_balance_days"`
	Assets    []string `json:"assets"`     // asset tags in the IT system
	CardID    string   `json:"card_id"`    // access card id
	PayrollID string   `json:"payroll_id"`
}

// ---------------------------------------------------------------- case & steps

type StepKind string

const (
	StepAgent      StepKind = "agent"
	StepRule       StepKind = "rule"
	StepApproval   StepKind = "approval"
	StepEffect     StepKind = "effect"
	StepHumanInput StepKind = "human_input"
)

type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepWaiting StepStatus = "waiting" // waiting for a person
	StepDone    StepStatus = "done"
	StepInDoubt StepStatus = "in_doubt"
	StepSkipped StepStatus = "skipped"
)

// StepState is the live state of one step of one case.
type StepState struct {
	ID        string     `json:"id"`
	N         int        `json:"n"`
	Status    StepStatus `json:"status"`
	Actor     string     `json:"actor,omitempty"`      // who completed it
	HumanTouch bool      `json:"human_touch"`          // true if a person acted
	Evidence  string     `json:"evidence,omitempty"`   // evidence level of its effect
	Note      string     `json:"note,omitempty"`
	DoneAt    time.Time  `json:"done_at,omitempty"`
}

type CaseStatus string

const (
	CaseOpen   CaseStatus = "open"
	CaseClosed CaseStatus = "closed"
)

// Case is one exit (or onboarding) journey.
type Case struct {
	ID         string                `json:"id"`
	Package    string                `json:"package"` // extension package name
	EmployeeID string                `json:"employee_id"`
	Status     CaseStatus            `json:"status"`
	OpenedAt   time.Time             `json:"opened_at"`
	ClosedAt   time.Time             `json:"closed_at,omitempty"`
	TenderDate string                `json:"tender_date,omitempty"` // resignation date YYYY-MM-DD
	RequestedLWD string              `json:"requested_lwd,omitempty"`
	BoundLWD   string                `json:"bound_lwd,omitempty"` // fixed by the approver's mandate
	Buyout     bool                  `json:"buyout,omitempty"`
	Facts      map[string]any        `json:"facts,omitempty"` // rule outputs, reconciliation results
	Steps      map[string]*StepState `json:"steps"`
	Sealed     bool                  `json:"sealed,omitempty"` // disclosure routed to ICC, text sealed
}

// ---------------------------------------------------------------- approvals & mandates

type ApprovalStatus string

const (
	ApprovalOpen     ApprovalStatus = "open"
	ApprovalGranted  ApprovalStatus = "granted"
	ApprovalRejected ApprovalStatus = "rejected"
)

// Approval is a reserved decision waiting for its named person.
type Approval struct {
	ID         string         `json:"id"`
	CaseID     string         `json:"case_id"`
	StepID     string         `json:"step_id"`
	ReservedID string         `json:"reserved_id"` // RSV-1, RSV-6, ...
	ApproverID string         `json:"approver_id"` // resolved from the org chart
	Status     ApprovalStatus `json:"status"`
	// Request is the exact, typed request being approved. Its canonical
	// hash is what the mandate binds. Changing one rupee changes the hash.
	Request map[string]any `json:"request"`
	Facts   []Fact         `json:"facts,omitempty"`
	Created time.Time      `json:"created"`
	Decided time.Time      `json:"decided,omitempty"`
	Reason  string         `json:"reason,omitempty"`
}

// Fact is one line of the approver's fact sheet, with its source.
type Fact struct {
	Label  string `json:"label"`
	Value  string `json:"value"`
	Source string `json:"source"` // which system or rule produced it
}

// Mandate is a named person's signed approval of exactly one request.
type Mandate struct {
	ID         string    `json:"id"`
	ApprovalID string    `json:"approval_id"`
	CaseID     string    `json:"case_id"`
	ReservedID string    `json:"reserved_id"`
	ApproverID string    `json:"approver_id"`
	// RequestHash is the canonical SHA-256 of the approved request.
	RequestHash string    `json:"request_hash"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	SingleUse   bool      `json:"single_use"`
	UsedAt      time.Time `json:"used_at,omitempty"`
	// ReAuth records that the approver re-authenticated for this exact
	// request (demo IdP stand-in; nonce = request hash).
	ReAuth string `json:"re_auth"`
}

// ---------------------------------------------------------------- intents & dispatch

type IntentState string

const (
	IntentPending   IntentState = "PENDING"
	IntentCommitted IntentState = "COMMITTED"
	IntentInDoubt   IntentState = "IN_DOUBT"
	IntentFailed    IntentState = "FAILED"
)

// Intent is one business intent against an outside system: at most one
// external effect may stand per intent key, within the stated fault model.
type Intent struct {
	Key       string      `json:"key"` // canonical hash: case|step|action|args
	CaseID    string      `json:"case_id"`
	StepID    string      `json:"step_id"`
	Action    string      `json:"action"` // e.g. directory.disable
	Target    string      `json:"target"` // mock service name
	Args      map[string]any `json:"args"`
	State     IntentState `json:"state"`
	Attempts  int         `json:"attempts"`
	MandateID string      `json:"mandate_id,omitempty"`
	ExternalRef string    `json:"external_ref,omitempty"` // id in the target system
	Evidence  string      `json:"evidence,omitempty"`     // evidence level reached
	Note      string      `json:"note,omitempty"`
	Updated   time.Time   `json:"updated"`
}

// Evidence levels, weakest to strongest. A receipt says which level its
// "done" rests on, and a clock declares the level it needs to close.
const (
	EvReplyOnly  = "reply_only"  // the target's reply alone
	EvObserved   = "observed"    // the record exists, but cannot be tied to our request
	EvAttributed = "attributed"  // the target's record carries our reference
)

// InDoubtItem is a step whose outcome could not be established. A named
// person owns it; nothing is retried blindly.
type InDoubtItem struct {
	ID       string    `json:"id"`
	CaseID   string    `json:"case_id"`
	StepID   string    `json:"step_id"`
	IntentKey string   `json:"intent_key,omitempty"`
	OwnerRole string   `json:"owner_role"` // hrops | payroll ...
	Reason   string    `json:"reason"`
	Evidence []Fact    `json:"evidence,omitempty"`
	Open     bool      `json:"open"`
	Created  time.Time `json:"created"`
	Resolved time.Time `json:"resolved,omitempty"`
	Resolution string  `json:"resolution,omitempty"`
	ResolvedBy string  `json:"resolved_by,omitempty"`
}

// ---------------------------------------------------------------- obligations (clocks)

type ClockStatus string

const (
	ClockOpen    ClockStatus = "open"
	ClockAtRisk  ClockStatus = "at_risk"
	ClockClosed  ClockStatus = "closed"
	ClockBreached ClockStatus = "breached"
)

// Obligation is a legal or policy deadline: a clock that closes only on
// evidence at its declared level.
type Obligation struct {
	ID        string      `json:"id"`
	CaseID    string      `json:"case_id"`
	RuleID    string      `json:"rule_id"`
	Title     string      `json:"title"`
	Basis     string      `json:"basis"` // citation
	NeedsLegalConfirmation bool `json:"needs_legal_confirmation"`
	StartEvent string     `json:"start_event"` // receipt action that started it
	StartDate  string     `json:"start_date"`  // YYYY-MM-DD
	DueDate    string     `json:"due_date"`    // YYYY-MM-DD
	SkippedDays []string  `json:"skipped_days,omitempty"` // non-working days in between
	EvidenceRequired string `json:"evidence_required"`    // attributed | observed
	ClosesOnAction   string `json:"closes_on_action"`     // receipt action that may close it
	Status    ClockStatus `json:"status"`
	ClosedBy  int64       `json:"closed_by_receipt,omitempty"` // receipt seq
	ClosedAt  time.Time   `json:"closed_at,omitempty"`
}

// ---------------------------------------------------------------- tasks

// Task is a waiting human input (handover confirmation, exit survey).
type Task struct {
	ID      string    `json:"id"`
	CaseID  string    `json:"case_id"`
	StepID  string    `json:"step_id"`
	Role    string    `json:"role"`
	UserID  string    `json:"user_id,omitempty"`
	Title   string    `json:"title"`
	Open    bool      `json:"open"`
	Created time.Time `json:"created"`
	DoneBy  string    `json:"done_by,omitempty"`
	DoneAt  time.Time `json:"done_at,omitempty"`
}
