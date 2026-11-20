// Read-side queries for the product surfaces that browse the record:
// people, single receipts, and live reads of the target systems.
package kernel

import (
	"fmt"
	"net/url"
	"sort"

	"counterfoil/internal/core"
)

// Employees lists the synthetic workforce, sorted by name.
func (a *App) Employees() []core.Employee {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]core.Employee, 0, len(a.employees))
	for _, e := range a.employees {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ReceiptBySeq returns one receipt from the chain.
func (a *App) ReceiptBySeq(seq int64) (core.Receipt, bool) {
	for _, r := range a.Receipts(0, "") {
		if r.Seq == seq {
			return r, true
		}
	}
	return core.Receipt{}, false
}

// LiveDirectory reads the directory stand-in's own record for an email —
// the same read the dispatcher's read-back performs, offered as a browse.
func (a *App) LiveDirectory(email string) (map[string]any, error) {
	if email == "" {
		return nil, fmt.Errorf("no email on record")
	}
	return a.mockGet("/directory/users/" + url.PathEscape(email))
}

// LiveDirectoryLookup distinguishes the three outcomes a security check
// must never conflate: found, verifiably absent (the target itself said
// 404), and UNKNOWN (unreachable, misbehaving). Absence is a verdict
// only when the target's own reply says so.
func (a *App) LiveDirectoryLookup(email string) (rec map[string]any, found bool, err error) {
	if email == "" {
		return nil, false, nil
	}
	rec, err = a.mockGet("/directory/users/" + url.PathEscape(email))
	if err == nil {
		return rec, true, nil
	}
	if err.Error() == "status 404" {
		return nil, false, nil
	}
	return nil, false, err
}

// LiveCard reads the access-card stand-in's own record for a card id.
func (a *App) LiveCard(cardID string) (map[string]any, error) {
	if cardID == "" {
		return nil, fmt.Errorf("no card on record")
	}
	return a.mockGet("/cards/cards/" + url.PathEscape(cardID))
}
