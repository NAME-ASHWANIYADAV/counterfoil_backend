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
	"sync"
	"time"

	"counterfoil/internal/core"
)

type Server struct {
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	mux   *http.ServeMux
	token string // optional shared secret (CFOIL_SIGNER_TOKEN)

	mu     sync.Mutex
	signed map[int64]string // seq -> head hash already countersigned
	maxSeq int64
	log    *os.File
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
	s := &Server{priv: priv, pub: priv.Public().(ed25519.PublicKey), mux: http.NewServeMux(),
		token: os.Getenv("CFOIL_SIGNER_TOKEN"), signed: map[int64]string{}}
	// The signer remembers what it has signed: it refuses to countersign a
	// different history for a sequence it already signed, and refuses to
	// go backwards — a compromised caller cannot quietly re-sign a
	// rewritten past.
	signedPath := filepath.Join(dataDir, "signed.jsonl")
	rows, _ := core.ReadAll[signedRow](signedPath)
	for _, row := range rows {
		s.signed[row.Seq] = row.Head
		if row.Seq > s.maxSeq {
			s.maxSeq = row.Seq
		}
	}
	f, err := os.OpenFile(signedPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.log = f
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	s.mux.HandleFunc("GET /pub", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, hex.EncodeToString(s.pub))
	})
	s.mux.HandleFunc("POST /sign", s.sign)
	return s, nil
}

type signedRow struct {
	Seq  int64  `json:"seq"`
	Head string `json:"head"`
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) sign(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, "unauthorised", 401)
		return
	}
	var in struct {
		UptoSeq  int64  `json:"upto_seq"`
		HeadHash string `json:"head_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.UptoSeq < 1 || len(in.HeadHash) != 64 {
		http.Error(w, "bad request", 400)
		return
	}
	s.mu.Lock()
	if prev, ok := s.signed[in.UptoSeq]; ok && prev != in.HeadHash {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("REFUSED: seq %d was already countersigned with a different head — history does not get rewritten", in.UptoSeq), 409)
		return
	}
	if in.UptoSeq < s.maxSeq {
		if prev, ok := s.signed[in.UptoSeq]; !ok || prev != in.HeadHash {
			s.mu.Unlock()
			http.Error(w, fmt.Sprintf("REFUSED: seq %d is behind the furthest countersigned seq %d", in.UptoSeq, s.maxSeq), 409)
			return
		}
	}
	if _, ok := s.signed[in.UptoSeq]; !ok {
		s.signed[in.UptoSeq] = in.HeadHash
		if in.UptoSeq > s.maxSeq {
			s.maxSeq = in.UptoSeq
		}
		b, _ := json.Marshal(signedRow{Seq: in.UptoSeq, Head: in.HeadHash})
		_, _ = s.log.Write(append(b, '\n'))
		_ = s.log.Sync()
	}
	s.mu.Unlock()
	sig := ed25519.Sign(s.priv, core.CheckpointBody(in.UptoSeq, in.HeadHash))
	cp := core.Checkpoint{UptoSeq: in.UptoSeq, HeadHash: in.HeadHash, TS: time.Now().UTC(),
		Sig: hex.EncodeToString(sig), Pub: hex.EncodeToString(s.pub)}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cp)
}
