package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"counterfoil/internal/core"
)

// Router places the AI. Rules decide dates, money, approvers and routing;
// a model only reads and drafts text, and only when a key is configured.
// Every model call is metered and receipted. With no key the product runs
// whole — nothing pretends: surfaces say "rules" or "model" truthfully.
type Router struct {
	app      *App
	ai       *AIClient // Gemini — the primary provider (CFOIL_AI_API_KEY)
	apiKey   string    // Anthropic — optional alternative for drafting only
	model    string
	endpoint string
	client   *http.Client
}

func NewRouter(a *App) *Router {
	return &Router{
		app:      a,
		ai:       NewAIClient(a),
		apiKey:   os.Getenv("ANTHROPIC_API_KEY"),
		model:    envOr("CFOIL_MODEL", "claude-haiku-4-5"),
		endpoint: envOr("CFOIL_ANTHROPIC_URL", "https://api.anthropic.com/v1/messages"),
		client:   &http.Client{Timeout: 20 * time.Second},
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// AI returns the agent-capable client, or nil when none is configured.
// Surfaces check this and honestly fall back to rules.
func (r *Router) AI() *AIClient { return r.ai }

func (r *Router) ModelConfigured() bool { return r.ai != nil || r.apiKey != "" }

func (r *Router) ModelName() string {
	switch {
	case r.ai != nil:
		return r.ai.Model()
	case r.apiKey != "":
		return r.model
	}
	return "none configured"
}

// ParseIntake extracts the resignation intent and requested date with
// rules (regex + date formats). Typed slots never come from a model.
// A question is never a tender: "What is my notice period?" goes to
// Q&A (or the agent), not to the intake card.
func (r *Router) ParseIntake(text string) (isResignation bool, requestedLWD string) {
	lt := strings.ToLower(strings.TrimSpace(text))
	if !containsAny(lt, []string{"resign", "quit", "leaving", "notice", "last working day", "put down my papers"}) {
		return false, ""
	}
	if strings.HasSuffix(lt, "?") || containsPrefix(lt, []string{"what", "how", "when", "why", "where", "which", "who",
		"does ", "do ", "is ", "are ", "can ", "could ", "will ", "would ", "should ", "tell me", "explain"}) {
		return false, ""
	}
	for _, layout := range []string{"2006-01-02", "2 January 2006", "2 Jan 2006", "January 2, 2006", "02/01/2006"} {
		for _, tok := range candidateDates(text) {
			if t, err := time.Parse(layout, tok); err == nil {
				return true, t.Format("2006-01-02")
			}
		}
	}
	if containsAny(lt, []string{"immediately", "today", "asap"}) {
		return true, fdate(time.Now())
	}
	return true, ""
}

func containsPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func candidateDates(text string) []string {
	words := strings.Fields(strings.NewReplacer(",", " ", ".", " ").Replace(text))
	var out []string
	for i := range words {
		for n := 1; n <= 3 && i+n <= len(words); n++ {
			out = append(out, strings.Join(words[i:i+n], " "))
		}
	}
	return out
}

// Acknowledge drafts the chat acknowledgement. Model if configured (and
// metered), a fixed template otherwise. The returned source is shown in
// the interface — never pretend.
func (r *Router) Acknowledge(empName, lwdDisplay string) (text, source string) {
	fallback := fmt.Sprintf(
		"Thank you, %s. I have recorded your resignation request — it is acknowledged, not yet accepted. "+
			"A named approver decides acceptance and your release date. Requested last working day: %s.",
		empName, lwdDisplay)
	if !r.ModelConfigured() {
		return fallback, "rules (no model configured)"
	}
	system := "You acknowledge an employee's resignation message in two warm, plain sentences. " +
		"Say it is acknowledged, not accepted, and that a named approver decides. Do not add dates, amounts or promises."
	user := fmt.Sprintf("Employee: %s. Requested last working day: %s.", empName, lwdDisplay)
	if r.ai != nil {
		out, err := r.ai.GenerateText("intake acknowledgement; typed slots stay rule-computed", system, user)
		if err != nil {
			return fallback, "rules (model call failed: " + err.Error() + ")"
		}
		return out, "model: " + r.ai.Model()
	}
	out, err := r.anthropic(system, user)
	if err != nil {
		return fallback, "rules (model call failed: " + err.Error() + ")"
	}
	return out, "model: " + r.model
}

func (r *Router) anthropic(system, user string) (string, error) {
	body := map[string]any{
		"model": r.model, "max_tokens": 300, "system": system,
		"messages": []map[string]any{{"role": "user", "content": user}},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", r.endpoint, bytes.NewReader(b))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", r.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("anthropic status %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	_ = r.app.emit(core.Receipt{
		Action: "model.call", ActorType: "agent", ActorID: "router",
		Data: map[string]any{"provider": "anthropic", "model": r.model,
			"tokens":  float64(out.Usage.InputTokens + out.Usage.OutputTokens),
			"purpose": "draft text only; typed slots stay rule-computed"},
	})
	var sb strings.Builder
	for _, c := range out.Content {
		sb.WriteString(c.Text)
	}
	return strings.TrimSpace(sb.String()), nil
}

// mockGet is a read against a target stand-in.
func (a *App) mockGet(path string) (map[string]any, error) {
	client := &http.Client{Timeout: a.cfg.DispatchTimeout}
	resp, err := client.Get(a.cfg.MocksURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var m map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}
