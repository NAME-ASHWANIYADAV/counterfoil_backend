package kernel

import (
	"strings"
	"testing"
	"time"

	"counterfoil/internal/core"
)

func testRules(t *testing.T) *RulePack {
	t.Helper()
	rp, err := LoadRules("../../rules/rulepack.json")
	if err != nil {
		t.Fatal(err)
	}
	return rp
}

// Working-day arithmetic: Fri 20 Nov 2026 + 2 working days skips the
// weekend AND the sample holiday on Tue 24 Nov, landing on Wed 25 Nov.
func TestFinalWagesClockSkipsNonWorkingDays(t *testing.T) {
	rp := testRules(t)
	start, _ := time.Parse("2006-01-02", "2026-11-20")
	due, skipped := rp.AddWorkingDays(start, rp.FinalWages.WorkingDays)
	if got := due.Format("2006-01-02"); got != "2026-11-25" {
		t.Fatalf("due = %s, want 2026-11-25 (skipped: %v)", got, skipped)
	}
	if len(skipped) != 3 {
		t.Fatalf("want 3 skipped days (Sat, Sun, holiday), got %v", skipped)
	}
}

// The settlement is rule-computed and every line carries a citation.
func TestFFLinesAreCited(t *testing.T) {
	rp := testRules(t)
	emp := core.Employee{ID: "E", JoinDate: "2023-03-01", MonthlyGross: 160000,
		BasicShare: 0.4, LeaveBalanceDays: 8}
	ff, err := rp.ComputeFF(emp, "2026-11-20", true, 320000)
	if err != nil {
		t.Fatal(err)
	}
	if ff.GratuityEligible {
		t.Fatal("3.7 years of service must not be gratuity-eligible under the demo rule")
	}
	for _, l := range ff.Lines {
		if strings.TrimSpace(l.Cite) == "" {
			t.Fatalf("line %q has no citation", l.Label)
		}
	}
}

// The admission check refuses approval-by-silence and an unapproved
// reserved effect — the rules a hostile package would trip.
func TestCheckRefusesHostilePackage(t *testing.T) {
	m := &Manifest{Format: "counterfoil.package/v1", Name: "hostile", Version: "0",
		Steps: []StepDef{
			{ID: "a", N: 1, Kind: core.StepApproval, Reserved: "RSV-9", ApproverRole: "manager",
				TimeoutApprove: 48},
			{ID: "b", N: 2, Kind: core.StepEffect, Auto: true,
				Effect: &EffectDef{Action: "payroll.release", Target: "payroll",
					ReadBack: ReadBack{Strategy: "attributed", Collection: "payroll/payments"},
					RequiredEvidence: "attributed"}},
		}}
	res := CheckManifest(m, func(action string) (ReservedDef, bool) {
		if action == "payroll.release" {
			return ReservedDef{ID: "RSV-6", Action: action}, true
		}
		return ReservedDef{}, false
	})
	if res.Errors < 2 {
		t.Fatalf("want at least 2 errors (silence rule, unmandated reserved effect), got %d: %+v",
			res.Errors, res.Findings)
	}
	if res.Cert != "" {
		t.Fatal("a refused package must not get a certificate")
	}
}

// A clock only closes on evidence at its declared level.
func TestEvidenceRanking(t *testing.T) {
	if evidenceAtLeast(core.EvReplyOnly, core.EvAttributed) {
		t.Fatal("a bare reply must never satisfy an attributed clock")
	}
	if !evidenceAtLeast(core.EvAttributed, core.EvObserved) {
		t.Fatal("attributed satisfies observed")
	}
}
