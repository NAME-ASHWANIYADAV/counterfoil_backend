// Package signersrv is cfoil-signer: a separate process that alone holds
// the checkpoint signing key. cfoil can append receipts without it, but
// only this process can countersign the chain head — so a compromised web
// process cannot rewrite history and silently re-sign it.
package signersrv

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"counterfoil/internal/core"
)

type Server struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	mux  *http.ServeMux
}

func New(dataDir string) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dataDir, "signer_key.hex")
	var priv ed25519.PrivateKey
	if b, err := os.ReadFile(keyPath); err == nil {
		raw, err := hex.DecodeString(string(b))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("signer key file is corrupt")
		}
		priv = ed25519.PrivateKey(raw)
	} else {
		_, p, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		priv = p
		if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(p)), 0o600); err != nil {
			return nil, err
		}
	}
	s := &Server{priv: priv, pub: priv.Public().(ed25519.PublicKey), mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	s.mux.HandleFunc("GET /pub", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, hex.EncodeToString(s.pub))
	})
	s.mux.HandleFunc("POST /sign", s.sign)
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) sign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UptoSeq  int64  `json:"upto_seq"`
		HeadHash string `json:"head_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.UptoSeq < 1 || len(in.HeadHash) != 64 {
		http.Error(w, "bad request", 400)
		return
	}
	sig := ed25519.Sign(s.priv, core.CheckpointBody(in.UptoSeq, in.HeadHash))
	cp := core.Checkpoint{UptoSeq: in.UptoSeq, HeadHash: in.HeadHash, TS: time.Now().UTC(),
		Sig: hex.EncodeToString(sig), Pub: hex.EncodeToString(s.pub)}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cp)
}
