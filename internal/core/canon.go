// Package core holds Counterfoil's domain types, the canonical hash, the
// append-only stores, the receipt chain and the intent ledger.
//
// Everything that claims "at most once" or "verifiable offline" lives here,
// so it can be read in one sitting and checked by the audit tool.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CanonicalJSON renders v with object keys sorted at every depth and no
// insignificant whitespace, so the same value always hashes the same.
// It is the form every mandate and receipt hash is computed over.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var any1 any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&any1); err != nil {
		return nil, err
	}
	var b strings.Builder
	if err := writeCanon(&b, any1); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeCanon(b *strings.Builder, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(t.String())
	case string:
		enc, err := json.Marshal(t)
		if err != nil {
			return err
		}
		b.Write(enc)
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanon(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			enc, _ := json.Marshal(k)
			b.Write(enc)
			b.WriteByte(':')
			if err := writeCanon(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("canon: unsupported type %T", v)
	}
	return nil
}

// Hash returns the hex SHA-256 of b.
func Hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// HashCanon canonicalises v and hashes it.
func HashCanon(v any) (string, error) {
	c, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return Hash(c), nil
}

// MustHashCanon is HashCanon for values built by our own code.
func MustHashCanon(v any) string {
	h, err := HashCanon(v)
	if err != nil {
		panic(err)
	}
	return h
}

// Short returns the first 16 hex characters, for display.
func Short(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
