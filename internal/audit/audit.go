// Package audit re-checks an exported receipt bundle offline. It never
// talks to the running system: everything it needs is in the bundle, plus
// the signer's public key inside the checkpoints. "Verifiable offline
// given the platform's signer keys" — this is that sentence, runnable.
package audit

import (
	"fmt"
	"path/filepath"
	"strings"

	"counterfoil/internal/core"
)

type Check struct {
	N      int    `json:"n"`
	Title  string `json:"title"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

func Run(dir string) ([]Check, error) {
	receipts, err := core.ReadAll[core.Receipt](filepath.Join(dir, "receipts.jsonl"))
	if err != nil {
		return nil, err
	}
	cps, err := core.ReadAll[core.Checkpoint](filepath.Join(dir, "checkpoints.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Check
	add := func(n int, title string, pass bool, detail string) {
		out = append(out, Check{n, title, pass, detail})
	}

	// 1. The chain itself.
	seq, head, err := core.VerifyChain(receipts)
	add(1, "Hash chain: every receipt links to the one before and hashes to itself",
		err == nil, map[bool]string{true: fmt.Sprintf("%d receipts, head %s", seq, core.Short(head)),
			false: fmt.Sprint(err)}[err == nil])

	// 2. Checkpoint signatures.
	badCp := 0
	detail := "no checkpoints in the bundle"
	for _, cp := range cps {
		if err := core.VerifyCheckpoint(cp, receipts); err != nil {
			badCp++
			detail = err.Error()
		}
	}
	if len(cps) > 0 && badCp == 0 {
		detail = fmt.Sprintf("%d checkpoint signatures verify against the chain", len(cps))
	}
	add(2, "Signer checkpoints: every countersignature verifies", len(cps) > 0 && badCp == 0, detail)

	// 3. Countersigned coverage.
	var lastCp int64
	for _, cp := range cps {
		if cp.UptoSeq > lastCp {
			lastCp = cp.UptoSeq
		}
	}
	gap := seq - lastCp
	add(3, "Coverage: how much history the signer has countersigned",
		lastCp > 0 && gap >= 0,
		fmt.Sprintf("countersigned up to seq %d of %d (%d receipts after the last checkpoint are chain-protected only)", lastCp, seq, gap))

	// Index receipts for the remaining checks.
	type mand struct {
		hash     string
		approver string
		actor    string
	}
	mandates := map[string]mand{}
	approvalsByHash := map[string]string{} // request_hash -> deciding actor
	var badActor []string
	for _, r := range receipts {
		switch r.Action {
		case "mandate.issued":
			if m, ok := r.Data["mandate"].(map[string]any); ok {
				id, _ := m["id"].(string)
				h, _ := m["request_hash"].(string)
				ap, _ := m["approver_id"].(string)
				mandates[id] = mand{hash: h, approver: ap, actor: r.ActorType}
			}
			if r.ActorType != "human" {
				badActor = append(badActor, fmt.Sprintf("mandate.issued at seq %d by %s", r.Seq, r.ActorType))
			}
		case "approval.decided":
			if r.ActorType != "human" {
				badActor = append(badActor, fmt.Sprintf("approval.decided at seq %d by %s", r.Seq, r.ActorType))
			}
			if h, ok := r.Data["request_hash"].(string); ok {
				approvalsByHash[h] = r.ActorID
			}
		}
	}

	// 4. Reserved dispatches carry a matching mandate.
	badMandate := []string{}
	reservedSeen := 0
	for _, r := range receipts {
		if r.Action != "effect.dispatched" && r.Action != "separation.accepted" && r.Action != "payment.released" {
			continue
		}
		mid, _ := r.Data["mandate_id"].(string)
		if r.Action == "effect.dispatched" {
			if mid == "" {
				continue // not a reserved effect
			}
		} else if mid == "" {
			if r.Action == "separation.accepted" {
				badMandate = append(badMandate, fmt.Sprintf("%s at seq %d has no mandate", r.Action, r.Seq))
			}
			continue
		}
		reservedSeen++
		m, ok := mandates[mid]
		if !ok {
			badMandate = append(badMandate, fmt.Sprintf("seq %d cites unknown mandate %s", r.Seq, mid))
			continue
		}
		if m.actor != "human" {
			badMandate = append(badMandate, fmt.Sprintf("mandate %s was not issued by a human receipt", mid))
		}
		if _, ok := approvalsByHash[m.hash]; !ok {
			badMandate = append(badMandate, fmt.Sprintf("mandate %s hash has no matching approval.decided", mid))
		}
	}
	add(4, "Reserved effects: every one cites a human-issued mandate whose hash matches a decided approval",
		len(badMandate) == 0, joinOr(badMandate, fmt.Sprintf("%d reserved effect(s) checked", reservedSeen)))

	// 5. No agent on reserved decisions.
	add(5, "No agent or system receipt ever decides an approval or issues a mandate",
		len(badActor) == 0, joinOr(badActor, "all approval and mandate receipts carry a human actor"))

	// 6. No silent Done on external effects: every dispatch is followed by
	// a read-back, a refusal, a retry or an in-doubt for the same key.
	follow := map[string]bool{}
	for _, r := range receipts {
		switch r.Action {
		case "effect.readback", "effect.refused", "effect.reconciled":
			if k, ok := r.Data["intent_key"].(string); ok {
				follow[k] = true
			}
		case "indoubt.opened":
			if item, ok := r.Data["item"].(map[string]any); ok {
				if k, ok := item["intent_key"].(string); ok && k != "" {
					follow[core.Short(k)] = true
				}
			}
		}
	}
	var silent []string
	for _, r := range receipts {
		if r.Action != "effect.dispatched" {
			continue
		}
		k, _ := r.Data["intent_key"].(string)
		if !follow[k] {
			silent = append(silent, fmt.Sprintf("dispatch at seq %d (%s) has no read-back, refusal or in-doubt", r.Seq, k))
		}
	}
	add(6, "No silent Done: every external dispatch ends in a read-back, a refusal or an in-doubt item",
		len(silent) == 0, joinOr(silent, "every dispatch is accounted for"))

	// 7. Clock closures carry the declared evidence.
	obNeed := map[string]string{}
	var badClock []string
	for _, r := range receipts {
		if r.Action == "obligation.started" {
			if ob, ok := r.Data["obligation"].(map[string]any); ok {
				id, _ := ob["id"].(string)
				need, _ := ob["evidence_required"].(string)
				obNeed[id] = need
			}
		}
		if r.Action == "obligation.closed" {
			id, _ := r.Data["obligation_id"].(string)
			need := obNeed[id]
			if !evidenceAtLeast(r.Evidence, need) {
				badClock = append(badClock, fmt.Sprintf("%s closed at seq %d with evidence %q below %q", id, r.Seq, r.Evidence, need))
			}
		}
	}
	add(7, "Clocks: every closed deadline closed on evidence at its declared level",
		len(badClock) == 0, joinOr(badClock, fmt.Sprintf("%d obligation(s) in the bundle", len(obNeed))))

	// 8. One dispatch per intent key, unless an explicit retry receipt sits between.
	counts := map[string]int{}
	retries := map[string]int{}
	for _, r := range receipts {
		if r.Action == "effect.dispatched" {
			if k, ok := r.Data["intent_key"].(string); ok {
				counts[k]++
			}
		}
		if r.Action == "effect.retried" {
			if k, ok := r.Data["intent_key"].(string); ok {
				retries[k]++
			}
		}
	}
	var dup []string
	for k, n := range counts {
		if n > 1+retries[k] {
			dup = append(dup, fmt.Sprintf("intent %s dispatched %d times with %d receipted retries", k, n, retries[k]))
		}
	}
	add(8, "Intent ledger: no intent key was dispatched twice without a receipted retry",
		len(dup) == 0, joinOr(dup, fmt.Sprintf("%d intent key(s) seen", len(counts))))

	// 9. Canaries: the last run of every probe held.
	lastHeld := map[string]bool{}
	for _, r := range receipts {
		if r.Action == "canary.run" {
			id, _ := r.Data["id"].(string)
			held, _ := r.Data["held"].(bool)
			lastHeld[id] = held
		}
	}
	var fell []string
	for id, held := range lastHeld {
		if !held {
			fell = append(fell, id+" did not hold on its last run")
		}
	}
	add(9, "Canaries: on their last run, every guard refused what it must refuse",
		len(fell) == 0, joinOr(fell, fmt.Sprintf("%d probe(s) on record", len(lastHeld))))

	// 10. Package activations: a passing check first, two named people.
	checked := map[string]bool{}
	var badAct []string
	for _, r := range receipts {
		if r.Action == "package.checked" {
			name, _ := r.Data["package"].(string)
			if e, ok := r.Data["errors"].(float64); ok && e == 0 {
				checked[name] = true
			}
		}
		if r.Action == "package.activated" {
			name, _ := r.Data["package"].(string)
			p1, _ := r.Data["proposed_by"].(string)
			p2, _ := r.Data["confirmed_by"].(string)
			if !checked[name] {
				badAct = append(badAct, name+" was activated without a passing admission check first")
			}
			if p1 == p2 && p1 != "bootstrap" {
				badAct = append(badAct, name+" was activated by one person alone")
			}
		}
	}
	add(10, "Packages: activation always follows a passing admission check, by two named people (bootstrap noted)",
		len(badAct) == 0, joinOr(badAct, "activations are in order"))

	return out, nil
}

func joinOr(bad []string, okMsg string) string {
	if len(bad) == 0 {
		return okMsg
	}
	if len(bad) > 4 {
		bad = append(bad[:4], fmt.Sprintf("… and %d more", len(bad)-4))
	}
	return strings.Join(bad, "; ")
}

func evidenceAtLeast(got, need string) bool {
	rank := map[string]int{"reply_only": 1, "observed": 2, "attributed": 3, "internal": 3}
	g, n := rank[got], rank[need]
	if n == 0 {
		return true
	}
	return g >= n
}
