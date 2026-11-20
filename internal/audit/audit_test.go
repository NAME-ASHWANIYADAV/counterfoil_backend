package audit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"counterfoil/internal/core"
)

// A bundle re-signed by a different key must fail once the real signer's
// key is pinned — the attack the pin exists for.
func TestPinnedKeyRejectsForeignSignature(t *testing.T) {
	dir := t.TempDir()
	ch, err := core.OpenChain(dir, "http://127.0.0.1:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	var last core.Receipt
	for i := 0; i < 3; i++ {
		last, err = ch.Append(core.Receipt{Action: "test.step"})
		if err != nil {
			t.Fatal(err)
		}
	}
	// An attacker signs the (possibly rewritten) chain with their own key.
	attackerPub, attackerPriv, _ := ed25519.GenerateKey(rand.Reader)
	cp := core.Checkpoint{UptoSeq: last.Seq, HeadHash: last.Hash, TS: time.Now(),
		Sig: hex.EncodeToString(ed25519.Sign(attackerPriv, core.CheckpointBody(last.Seq, last.Hash))),
		Pub: hex.EncodeToString(attackerPub)}
	b, _ := json.Marshal(cp)
	if err := os.WriteFile(filepath.Join(dir, "checkpoints.jsonl"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// Unpinned: the signature is internally consistent, so check 2 passes
	// (and says the key came from the bundle).
	checks, err := Run(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !checks[1].Pass {
		t.Fatalf("unpinned check 2 should pass on an internally consistent bundle: %s", checks[1].Detail)
	}

	// Pinned to the real signer's key: the foreign signature is rejected.
	realPub, _, _ := ed25519.GenerateKey(rand.Reader)
	checks, err = Run(dir, hex.EncodeToString(realPub))
	if err != nil {
		t.Fatal(err)
	}
	if checks[1].Pass {
		t.Fatal("a checkpoint signed by a foreign key must fail against the pinned key")
	}
}
