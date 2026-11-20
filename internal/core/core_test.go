package core

import (
	"path/filepath"
	"testing"
)

// The canonical form must not care about Go map order, and must change
// whenever a value changes — one rupee included.
func TestCanonicalHash(t *testing.T) {
	a := map[string]any{"action": "payroll.release", "args": map[string]any{"amount_inr": 100.0, "payee": "PAY-1"}}
	b := map[string]any{"args": map[string]any{"payee": "PAY-1", "amount_inr": 100.0}, "action": "payroll.release"}
	if MustHashCanon(a) != MustHashCanon(b) {
		t.Fatal("key order changed the hash")
	}
	c := map[string]any{"action": "payroll.release", "args": map[string]any{"amount_inr": 101.0, "payee": "PAY-1"}}
	if MustHashCanon(a) == MustHashCanon(c) {
		t.Fatal("one rupee did not change the hash")
	}
}

// A receipt chain verifies, and any edit to history breaks it.
func TestChainTamperDetected(t *testing.T) {
	dir := t.TempDir()
	ch, err := OpenChain(dir, "http://127.0.0.1:1", 0) // signer unreachable: appends must still work
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	for i := 0; i < 5; i++ {
		if _, err := ch.Append(Receipt{Action: "test.step", Data: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := ReadAll[Receipt](filepath.Join(dir, "receipts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyChain(rs); err != nil {
		t.Fatalf("clean chain must verify: %v", err)
	}
	rs[2].Data["i"] = 99.0 // rewrite history
	if _, _, err := VerifyChain(rs); err == nil {
		t.Fatal("a rewritten receipt must break verification")
	}
}

// The ledger gives out an intent key exactly once; a second begin returns
// the stored entry and never signals "send".
func TestLedgerSingleBegin(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	key := IntentKey("CASE", "step", "x.create", map[string]any{"n": 1})
	_, began1, _ := l.Begin(Intent{Key: key})
	_, began2, _ := l.Begin(Intent{Key: key})
	if !began1 || began2 {
		t.Fatalf("begin1=%v begin2=%v; want true,false", began1, began2)
	}
	// And the claim survives a restart (replay from disk).
	l2, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if _, began3, _ := l2.Begin(Intent{Key: key}); began3 {
		t.Fatal("a restart must not re-issue the same intent")
	}
}
