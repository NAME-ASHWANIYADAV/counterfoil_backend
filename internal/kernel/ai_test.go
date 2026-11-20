package kernel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateArgs(t *testing.T) {
	tool := AITool{
		Name: "t",
		Params: map[string]AIParam{
			"q":    {Type: "string"},
			"n":    {Type: "number"},
			"flag": {Type: "boolean"},
			"mode": {Type: "string", Enum: []string{"a", "b"}},
		},
		Required: []string{"q"},
	}
	cases := []struct {
		name string
		args map[string]any
		ok   bool
	}{
		{"all good", map[string]any{"q": "x", "n": float64(3), "flag": true, "mode": "a"}, true},
		{"missing required", map[string]any{"n": float64(1)}, false},
		{"unknown arg", map[string]any{"q": "x", "zzz": "y"}, false},
		{"wrong type string", map[string]any{"q": float64(1)}, false},
		{"wrong type number", map[string]any{"q": "x", "n": "three"}, false},
		{"wrong type bool", map[string]any{"q": "x", "flag": "yes"}, false},
		{"enum violation", map[string]any{"q": "x", "mode": "c"}, false},
		{"optional omitted", map[string]any{"q": "x"}, true},
	}
	for _, c := range cases {
		err := validateArgs(tool, c.args)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// A scripted provider: first turn asks for a tool (with one bad call the
// loop must refuse and report back), second turn answers in text. The
// loop must run the real tool exactly once, log every step honestly and
// return the grounded answer.
func TestRunAgentToolLoop(t *testing.T) {
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" {
			http.Error(w, "no key", 401)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		turn++
		w.Header().Set("Content-Type", "application/json")
		switch turn {
		case 1: // one invalid call (unknown tool) and one valid call
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{
					map[string]any{"functionCall": map[string]any{"name": "no_such_tool", "args": map[string]any{}}},
					map[string]any{"functionCall": map[string]any{"name": "get_policy", "args": map[string]any{"query": "notice"}}},
				}}}},
				"usageMetadata": map[string]any{"totalTokenCount": 10},
			})
		default: // the model must have been shown both results; now it answers
			contents, _ := req["contents"].([]any)
			last, _ := contents[len(contents)-1].(map[string]any)
			parts, _ := last["parts"].([]any)
			if len(parts) != 2 {
				t.Errorf("expected 2 functionResponse parts, got %d", len(parts))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{
					map[string]any{"text": "Your notice period is 60 days (HR Policy s.9)."},
				}}}},
				"usageMetadata": map[string]any{"totalTokenCount": 12},
			})
		}
	}))
	defer srv.Close()

	ran := 0
	ai := &AIClient{apiKey: "test-key", model: "test-model", endpoint: srv.URL,
		client: &http.Client{Timeout: 5 * time.Second}}
	tools := []AITool{{
		Name:     "get_policy",
		Desc:     "look up policy",
		Params:   map[string]AIParam{"query": {Type: "string"}},
		Required: []string{"query"},
		Run: func(args map[string]any) (map[string]any, error) {
			ran++
			if args["query"] != "notice" {
				t.Errorf("args = %v", args)
			}
			return map[string]any{"answer": "60 days", "cite": "HR Policy s.9"}, nil
		},
	}}
	final, steps, err := ai.RunAgent("test", "system", nil, "what is my notice period?", tools, 4)
	if err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatalf("tool ran %d times, want exactly 1", ran)
	}
	if final == "" {
		t.Fatal("no final text")
	}
	// Honest log: model, refused tool, successful tool, model.
	var kinds []string
	for _, s := range steps {
		kinds = append(kinds, s.Kind+":"+s.Name)
	}
	if len(steps) != 4 || steps[1].OK || !steps[2].OK {
		t.Fatalf("steps wrong: %v", kinds)
	}
}

// A provider that never settles must be cut off by the iteration cap,
// not loop forever.
func TestRunAgentIterationCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{"name": "ping", "args": map[string]any{}}},
			}}}},
		})
	}))
	defer srv.Close()
	ai := &AIClient{apiKey: "k", model: "m", endpoint: srv.URL, client: &http.Client{Timeout: 5 * time.Second}}
	calls := 0
	tools := []AITool{{Name: "ping", Desc: "d", Run: func(map[string]any) (map[string]any, error) {
		calls++
		return map[string]any{"pong": true}, nil
	}}}
	_, _, err := ai.RunAgent("test", "sys", nil, "go", tools, 3)
	if err == nil {
		t.Fatal("expected an error from the iteration cap")
	}
	if calls != 3 {
		t.Fatalf("tool ran %d times, want 3 (once per capped turn)", calls)
	}
}

// The fallback model is tried exactly once, on retriable failures only.
func TestFallbackModel(t *testing.T) {
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models = append(models, r.URL.Path)
		if len(models) == 1 {
			http.Error(w, "overloaded", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ok"}}}}},
			"usageMetadata": map[string]any{"totalTokenCount": 5},
		})
	}))
	defer srv.Close()
	ai := &AIClient{apiKey: "k", model: "primary", fallback: "backup", endpoint: srv.URL,
		client: &http.Client{Timeout: 5 * time.Second}}
	out, err := ai.GenerateText("test", "sys", "hi")
	if err != nil || out != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(models) != 2 || models[0] != "/models/primary:generateContent" || models[1] != "/models/backup:generateContent" {
		t.Fatalf("models hit: %v", models)
	}
}

// A question about notice is never a tender; a statement is.
func TestParseIntakeQuestionsAreNotTenders(t *testing.T) {
	r := &Router{}
	cases := []struct {
		text string
		res  bool
	}{
		{"What is my notice period?", false},
		{"what is my notice period", false},
		{"Does my leave balance pay out when I am leaving?", false},
		{"Can I resign over chat?", false},
		{"I wish to resign, my last working day should be 2026-12-30", true},
		{"I quit.", true},
		{"i am leaving on 30 December 2026", true},
		{"I hereby give notice effective 2026-12-30", true},
	}
	for _, c := range cases {
		got, _ := r.ParseIntake(c.text)
		if got != c.res {
			t.Errorf("%q: intake=%v, want %v", c.text, got, c.res)
		}
	}
}
