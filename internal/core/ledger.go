package core

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Ledger is the intent ledger: one entry per business intent. The entry is
// written PENDING and synced to disk *before* any bytes leave for the
// target system, so a crash can never lose the fact that we may have
// acted. State transitions are appended; the last line per key wins.
//
// This is the S1 spike's protocol in product form, with its limits kept:
// at most once per intent through our own crashes, if the target saves
// within our deadline; through freezes or splits only where the target
// enforces our request ID. We never claim more.
type Ledger struct {
	mu   sync.Mutex
	log  *Log
	last map[string]Intent
}

func OpenLedger(dir string) (*Ledger, error) {
	log, err := OpenLog(dir + "/intents.jsonl")
	if err != nil {
		return nil, err
	}
	l := &Ledger{log: log, last: map[string]Intent{}}
	rows, err := ReadAll[Intent](log.Path())
	if err != nil {
		return nil, err
	}
	for _, in := range rows {
		l.last[in.Key] = in
	}
	return l, nil
}

// Close releases the underlying file (tests and shutdown).
func (l *Ledger) Close() error { return l.log.Close() }

// IntentKey derives the ledger key for one business intent.
func IntentKey(caseID, stepID, action string, args map[string]any) string {
	return MustHashCanon(map[string]any{
		"case": caseID, "step": stepID, "action": action, "args": args,
	})
}

// Begin claims the intent. If the key already exists, the stored entry is
// returned with began=false: the caller must branch on its state and must
// NOT send again on PENDING or COMMITTED.
func (l *Ledger) Begin(in Intent) (Intent, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if got, ok := l.last[in.Key]; ok {
		return got, false, nil
	}
	in.State = IntentPending
	in.Attempts = 1
	in.Updated = time.Now().UTC()
	if err := l.log.Append(in); err != nil {
		return in, false, err
	}
	l.last[in.Key] = in
	return in, true, nil
}

// Transition appends a new state for the key.
func (l *Ledger) Transition(key string, mut func(*Intent)) (Intent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	in, ok := l.last[key]
	if !ok {
		return in, fmt.Errorf("ledger: unknown intent %s", Short(key))
	}
	mut(&in)
	in.Updated = time.Now().UTC()
	if err := l.log.Append(in); err != nil {
		return in, err
	}
	l.last[key] = in
	return in, nil
}

func (l *Ledger) Get(key string) (Intent, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	in, ok := l.last[key]
	return in, ok
}

// Pending returns intents still PENDING — the ones a restart must
// reconcile by asking the target, never by resending blindly.
func (l *Ledger) Pending() []Intent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Intent
	for _, in := range l.last {
		if in.State == IntentPending {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.Before(out[j].Updated) })
	return out
}

func (l *Ledger) All() []Intent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Intent, 0, len(l.last))
	for _, in := range l.last {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.Before(out[j].Updated) })
	return out
}
