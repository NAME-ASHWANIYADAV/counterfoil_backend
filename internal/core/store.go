package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Log is an append-only JSON-lines file. Every Append is flushed and synced
// before it returns: a record either reached disk or the call failed.
type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{path: path, f: f}, nil
}

func (l *Log) Append(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

func (l *Log) Path() string { return l.path }

// ReadLines calls fn for every line of a JSONL file, in order. A missing
// file is an empty log. A truncated final line (a crash mid-write) is
// reported once and skipped; everything before it is kept.
func ReadLines(path string, fn func(raw []byte) error) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			fmt.Fprintf(os.Stderr, "store: %s line %d is not valid JSON (crash mid-write?); skipped\n", path, n)
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		if err := fn(cp); err != nil {
			return fmt.Errorf("%s line %d: %w", path, n, err)
		}
	}
	return sc.Err()
}

// ReadAll decodes every line of a JSONL file into T.
func ReadAll[T any](path string) ([]T, error) {
	var out []T
	err := ReadLines(path, func(raw []byte) error {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	return out, err
}
