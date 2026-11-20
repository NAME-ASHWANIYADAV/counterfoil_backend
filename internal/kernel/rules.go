package kernel

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"counterfoil/internal/core"
)

// RulePack is the governed rule layer: dated, cited, testable data —
// never prompts. Dates and money come from here and from people, never
// from a model.
type RulePack struct {
	Version string `json:"version"`
	Dated   string `json:"dated"`
	Notice  struct {
		Days         int    `json:"days"`
		BuyoutAllowed bool  `json:"buyout_allowed"`
		Cite         string `json:"cite"`
	} `json:"notice"`
	FinalWages struct {
		WorkingDays int    `json:"working_days"`
		Cite        string `json:"cite"`
		NeedsLegalConfirmation bool `json:"needs_legal_confirmation"`
	} `json:"final_wages"`
	Gratuity struct {
		MinYears float64 `json:"min_years"`
		DueDays  int     `json:"due_days"`
		Cite     string  `json:"cite"`
		NeedsLegalConfirmation bool `json:"needs_legal_confirmation"`
	} `json:"gratuity"`
	RelievingLetter struct {
		Days int    `json:"days"`
		Cite string `json:"cite"`
	} `json:"relieving_letter"`
	LeaveEncashment struct {
		Cite string `json:"cite"`
	} `json:"leave_encashment"`
	Holidays map[string]string `json:"holidays"` // YYYY-MM-DD -> name (sample calendar)
	QA       []QAEntry         `json:"qa"`
}

// QAEntry answers a policy question with a citation — retrieval over the
// rule pack, no model call.
type QAEntry struct {
	Keywords []string `json:"keywords"`
	Answer   string   `json:"answer"`
	Cite     string   `json:"cite"`
}

func LoadRules(path string) (*RulePack, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rp RulePack
	if err := json.Unmarshal(b, &rp); err != nil {
		return nil, err
	}
	return &rp, nil
}

const day = 24 * time.Hour

func parseDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

func fdate(t time.Time) string { return t.Format("2006-01-02") }

// FDisplay renders 2026-12-19 as "Fri 19 Dec 2026".
func FDisplay(s string) string {
	t, err := parseDate(s)
	if err != nil {
		return s
	}
	return t.Format("Mon 2 Jan 2006")
}

func (rp *RulePack) isWorkingDay(t time.Time) (bool, string) {
	switch t.Weekday() {
	case time.Saturday:
		return false, "Saturday"
	case time.Sunday:
		return false, "Sunday"
	}
	if name, ok := rp.Holidays[fdate(t)]; ok {
		return false, name
	}
	return true, ""
}

// AddWorkingDays returns start+n working days and the list of days it
// skipped, so a clock can show its whole trace.
func (rp *RulePack) AddWorkingDays(start time.Time, n int) (time.Time, []string) {
	var skipped []string
	t := start
	for added := 0; added < n; {
		t = t.Add(day)
		if ok, why := rp.isWorkingDay(t); ok {
			added++
		} else {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", fdate(t), why))
		}
	}
	return t, skipped
}

// ---------------------------------------------------------------- notice

// NoticeFacts is the step-4 rule output.
type NoticeFacts struct {
	NoticeDays   int     `json:"notice_days"`
	TenderDate   string  `json:"tender_date"`
	EarliestLWD  string  `json:"earliest_lwd"`
	RequestedLWD string  `json:"requested_lwd"`
	ShortfallDays int    `json:"shortfall_days"`
	BuyoutAllowed bool   `json:"buyout_allowed"`
	BuyoutAmount  float64 `json:"buyout_amount_inr"`
	Cite         string  `json:"cite"`
}

func (rp *RulePack) ComputeNotice(emp core.Employee, tender, requestedLWD string) (NoticeFacts, error) {
	td, err := parseDate(tender)
	if err != nil {
		return NoticeFacts{}, fmt.Errorf("tender date: %w", err)
	}
	earliest := td.Add(time.Duration(rp.Notice.Days) * day)
	nf := NoticeFacts{
		NoticeDays: rp.Notice.Days, TenderDate: tender,
		EarliestLWD: fdate(earliest), RequestedLWD: requestedLWD,
		BuyoutAllowed: rp.Notice.BuyoutAllowed, Cite: rp.Notice.Cite,
	}
	if requestedLWD != "" {
		rd, err := parseDate(requestedLWD)
		if err != nil {
			return nf, fmt.Errorf("requested last working day: %w", err)
		}
		if rd.Before(earliest) {
			short := int(earliest.Sub(rd) / day)
			nf.ShortfallDays = short
			perDay := emp.MonthlyGross / 30
			nf.BuyoutAmount = math.Round(perDay * float64(short))
		}
	}
	return nf, nil
}

// ---------------------------------------------------------------- final settlement

// FFLine is one row of the full-and-final computation, with its citation.
type FFLine struct {
	Label  string  `json:"label"`
	Amount float64 `json:"amount_inr"` // negative = recovery
	Cite   string  `json:"cite"`
	Note   string  `json:"note,omitempty"`
}

type FFFacts struct {
	Lines   []FFLine `json:"lines"`
	Total   float64  `json:"total_inr"`
	Period  string   `json:"period"` // e.g. "Dec 2026"
	GratuityEligible bool `json:"gratuity_eligible"`
	ServiceYears     float64 `json:"service_years"`
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func (rp *RulePack) ComputeFF(emp core.Employee, boundLWD string, buyout bool, buyoutAmount float64) (FFFacts, error) {
	lwd, err := parseDate(boundLWD)
	if err != nil {
		return FFFacts{}, fmt.Errorf("last working day: %w", err)
	}
	join, err := parseDate(emp.JoinDate)
	if err != nil {
		return FFFacts{}, fmt.Errorf("join date: %w", err)
	}
	var ff FFFacts
	ff.Period = lwd.Format("Jan 2006")
	ff.ServiceYears = round2(lwd.Sub(join).Hours() / 24 / 365.25)

	// Pro-rata salary for the LWD month.
	daysWorked := lwd.Day()
	perDay := emp.MonthlyGross / 30
	ff.Lines = append(ff.Lines, FFLine{
		Label:  fmt.Sprintf("Salary %s, days 1–%d pro-rata", ff.Period, daysWorked),
		Amount: round2(perDay * float64(daysWorked)),
		Cite:   "Contract: monthly gross / 30 per day",
	})
	// Leave encashment.
	if emp.LeaveBalanceDays > 0 {
		ff.Lines = append(ff.Lines, FFLine{
			Label:  fmt.Sprintf("Earned-leave encashment, %.0f days", emp.LeaveBalanceDays),
			Amount: round2(perDay * emp.LeaveBalanceDays),
			Cite:   rp.LeaveEncashment.Cite,
		})
	}
	// Gratuity.
	if ff.ServiceYears >= rp.Gratuity.MinYears {
		ff.GratuityEligible = true
		basicMonthly := emp.MonthlyGross * emp.BasicShare
		years := math.Round(ff.ServiceYears)
		amt := round2(basicMonthly * 15 / 26 * years)
		ff.Lines = append(ff.Lines, FFLine{
			Label:  fmt.Sprintf("Gratuity, %.0f years at 15/26 of last basic", years),
			Amount: amt, Cite: rp.Gratuity.Cite, Note: "needs_legal_confirmation",
		})
	} else {
		ff.Lines = append(ff.Lines, FFLine{
			Label:  fmt.Sprintf("Gratuity: not eligible (service %.1f y < %.2f y)", ff.ServiceYears, rp.Gratuity.MinYears),
			Amount: 0, Cite: rp.Gratuity.Cite, Note: "needs_legal_confirmation",
		})
	}
	// Notice buyout recovery.
	if buyout && buyoutAmount > 0 {
		ff.Lines = append(ff.Lines, FFLine{
			Label:  "Notice-shortfall recovery (buyout agreed at acceptance)",
			Amount: -round2(buyoutAmount), Cite: rp.Notice.Cite,
		})
	}
	for _, l := range ff.Lines {
		ff.Total += l.Amount
	}
	ff.Total = round2(ff.Total)
	return ff, nil
}

// AsFF decodes a stored fact back into FFFacts (for the web layer).
func AsFF(v any) (FFFacts, bool) { return asJSON[FFFacts](v) }

// AnswerQA retrieves the best rule-pack answer for a question, with its
// citation. Pure keyword retrieval; no model call, and labelled so.
func (rp *RulePack) AnswerQA(q string) (QAEntry, bool) {
	lq := strings.ToLower(q)
	best, score := QAEntry{}, 0
	for _, e := range rp.QA {
		s := 0
		for _, k := range e.Keywords {
			if strings.Contains(lq, strings.ToLower(k)) {
				s++
			}
		}
		if s > score {
			best, score = e, s
		}
	}
	return best, score > 0
}
