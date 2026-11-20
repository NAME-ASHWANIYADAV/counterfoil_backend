package core

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Receipt is one state transition: the counterfoil the product is named
// after. Receipts are hash-chained in order; the signer co-signs
// checkpoints of the chain head so history up to a checkpoint cannot be
// rewritten without the keys. Personal data stays out: free text is
// hashed, never stored here.
type Receipt struct {
	Seq       int64          `json:"seq"`
	TS        time.Time      `json:"ts"`
	CaseID    string         `json:"case_id,omitempty"`
	StepID    string         `json:"step_id,omitempty"`
	Action    string         `json:"action"`     // e.g. case.opened, effect.dispatched
	ActorType string         `json:"actor_type"` // agent | human | system
	ActorID   string         `json:"actor_id,omitempty"`
	Evidence  string         `json:"evidence,omitempty"` // evidence level, where it applies
	Data      map[string]any `json:"data,omitempty"`     // typed, non-personal fields and hashes
	PrevHash  string         `json:"prev_hash"`
	Hash      string         `json:"hash"`
}

// Checkpoint is the signer's countersignature over the chain head.
type Checkpoint struct {
	UptoSeq  int64     `json:"upto_seq"`
	HeadHash string    `json:"head_hash"`
	TS       time.Time `json:"ts"`
	Sig      string    `json:"sig"` // hex ed25519 over canonical {upto_seq, head_hash}
	Pub      string    `json:"pub"` // hex public key
}

// CheckpointBody is the exact bytes the signer signs.
func CheckpointBody(uptoSeq int64, headHash string) []byte {
	b, _ := CanonicalJSON(map[string]any{"upto_seq": uptoSeq, "head_hash": headHash})
	return b
}

// Chain appends receipts, maintains the hash chain, and asks the separate
// signer process for a checkpoint every `every` receipts. The signer being
// down never blocks an append; the gap is visible and closed at the next
// checkpoint.
type Chain struct {
	mu        sync.Mutex
	log       *Log
	cpLog     *Log
	seq       int64
	head      string
	signerURL string
	every     int64
	client    *http.Client
	// observers are notified after a receipt is durably appended.
	observers []func(Receipt)
	lastCp    Checkpoint
	signerErr string
}

func OpenChain(dir string, signerURL string, every int64) (*Chain, error) {
	log, err := OpenLog(dir + "/receipts.jsonl")
	if err != nil {
		return nil, err
	}
	cp, err := OpenLog(dir + "/checkpoints.jsonl")
	if err != nil {
		return nil, err
	}
	c := &Chain{log: log, cpLog: cp, signerURL: signerURL, every: every,
		client: &http.Client{Timeout: 3 * time.Second}}
	// Replay to find the head.
	rs, err := ReadAll[Receipt](log.Path())
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		c.seq, c.head = r.Seq, r.Hash
	}
	cps, err := ReadAll[Checkpoint](cp.Path())
	if err != nil {
		return nil, err
	}
	if len(cps) > 0 {
		c.lastCp = cps[len(cps)-1]
	}
	return c, nil
}

func (c *Chain) Observe(fn func(Receipt)) { c.observers = append(c.observers, fn) }

// Checkpoints returns every countersignature on record, oldest first.
func (c *Chain) Checkpoints() []Checkpoint {
	cps, _ := ReadAll[Checkpoint](c.cpLog.Path())
	return cps
}

// Close releases the underlying files (tests and shutdown).
func (c *Chain) Close() error {
	err1 := c.log.Close()
	err2 := c.cpLog.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// Append computes the chain fields, writes the receipt durably, then
// notifies observers. It returns the completed receipt.
func (c *Chain) Append(r Receipt) (Receipt, error) {
	c.mu.Lock()
	r.Seq = c.seq + 1
	r.TS = time.Now().UTC()
	r.PrevHash = c.head
	if r.ActorType == "" {
		r.ActorType = "system"
	}
	r.Hash = receiptHash(r)
	if err := c.log.Append(r); err != nil {
		c.mu.Unlock()
		return r, err
	}
	c.seq, c.head = r.Seq, r.Hash
	needCp := c.every > 0 && r.Seq%c.every == 0
	obs := c.observers
	c.mu.Unlock()

	for _, fn := range obs {
		fn(r)
	}
	if needCp {
		c.RequestCheckpoint()
	}
	return r, nil
}

// ReceiptHash recomputes a receipt's own hash from its contents — the
// receipt page uses it to show, live, that the stored hash is not a
// decoration but a recomputable fact.
func ReceiptHash(r Receipt) string { return receiptHash(r) }

func receiptHash(r Receipt) string {
	cp := r
	cp.Hash = ""
	b, err := CanonicalJSON(cp)
	if err != nil {
		panic(err)
	}
	return Hash(b)
}

// RequestCheckpoint asks the signer to countersign the current head.
func (c *Chain) RequestCheckpoint() {
	c.mu.Lock()
	seq, head := c.seq, c.head
	c.mu.Unlock()
	if seq == 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{"upto_seq": seq, "head_hash": head})
	req, _ := http.NewRequest("POST", c.signerURL+"/sign", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("CFOIL_SIGNER_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.mu.Lock()
		c.signerErr = err.Error()
		c.mu.Unlock()
		return
	}
	defer resp.Body.Close()
	var cp Checkpoint
	if err := json.NewDecoder(resp.Body).Decode(&cp); err != nil || resp.StatusCode != 200 {
		c.mu.Lock()
		c.signerErr = fmt.Sprintf("signer status %d", resp.StatusCode)
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.cpLog.Append(cp); err != nil {
		c.signerErr = err.Error()
		return
	}
	c.lastCp, c.signerErr = cp, ""
}

func (c *Chain) Head() (int64, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq, c.head
}

func (c *Chain) LastCheckpoint() (Checkpoint, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastCp, c.signerErr
}

func (c *Chain) Paths() (receipts, checkpoints string) {
	return c.log.Path(), c.cpLog.Path()
}

// VerifyChain recomputes every hash and link. It is pure: the audit tool
// uses it offline. It returns the number of receipts and the head.
func VerifyChain(receipts []Receipt) (int64, string, error) {
	prev := ""
	var seq int64
	for i, r := range receipts {
		if r.Seq != int64(i+1) {
			return 0, "", fmt.Errorf("receipt %d: seq %d out of order", i+1, r.Seq)
		}
		if r.PrevHash != prev {
			return 0, "", fmt.Errorf("receipt seq %d: prev_hash mismatch", r.Seq)
		}
		if receiptHash(r) != r.Hash {
			return 0, "", fmt.Errorf("receipt seq %d: hash mismatch (content changed?)", r.Seq)
		}
		prev, seq = r.Hash, r.Seq
	}
	return seq, prev, nil
}

// VerifyCheckpoint checks one countersignature against the chain.
// pinnedPub, when non-empty, is the signer's public key obtained outside
// the bundle (a flag, a config, or the signer itself): any checkpoint
// carrying a different key is rejected, so an attacker who rewrites the
// bundle cannot simply re-sign it with their own key.
func VerifyCheckpoint(cp Checkpoint, receipts []Receipt, pinnedPub string) error {
	if cp.UptoSeq < 1 || cp.UptoSeq > int64(len(receipts)) {
		return fmt.Errorf("checkpoint seq %d outside chain", cp.UptoSeq)
	}
	r := receipts[cp.UptoSeq-1]
	if r.Hash != cp.HeadHash {
		return fmt.Errorf("checkpoint at seq %d: head hash does not match the chain", cp.UptoSeq)
	}
	if pinnedPub != "" && !strings.EqualFold(cp.Pub, pinnedPub) {
		return fmt.Errorf("checkpoint at seq %d: signed with %s, not the pinned signer key %s", cp.UptoSeq, Short(cp.Pub), Short(pinnedPub))
	}
	pub, err := hex.DecodeString(cp.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("checkpoint at seq %d: bad public key", cp.UptoSeq)
	}
	sig, err := hex.DecodeString(cp.Sig)
	if err != nil {
		return fmt.Errorf("checkpoint at seq %d: bad signature encoding", cp.UptoSeq)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), CheckpointBody(cp.UptoSeq, cp.HeadHash), sig) {
		return fmt.Errorf("checkpoint at seq %d: signature does not verify", cp.UptoSeq)
	}
	return nil
}
