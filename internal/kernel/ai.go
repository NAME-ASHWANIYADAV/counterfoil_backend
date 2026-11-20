// The AI service. One place holds the provider client, the model choice,
// the tool-calling loop and the guard rails; every surface that wants a
// model goes through here, and every call leaves a model.call receipt.
//
// Placement doctrine, unchanged: the model READS and DRAFTS. It reaches
// real application data only through typed, server-defined tools; it
// cannot name a URL, decide an approval, move money or close a clock.
// Words cannot decide — tools that change anything only STAGE work that
// a named person confirms.
//
// Provider: Google Gemini (CFOIL_AI_API_KEY). The key is read from the
// environment server-side, sent in a request header, and never appears
// in a URL, a log line, a template or a receipt. Models were chosen from
// the provider's live model list by measured capability on our actual
// needs — verified function calling, a grounded tool round-trip and
// schema-constrained JSON, at interactive latency — not by version
// number; see README "Where the AI is".
package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"counterfoil/internal/core"
)

// ---------------------------------------------------------------- client

type AIClient struct {
	app      *App
	apiKey   string
	model    string // primary, e.g. gemini-2.5-flash
	fallback string // tried once on 429/5xx/transport failure; "" disables
	endpoint string // base URL of the Gemini v1beta API
	client   *http.Client
}

// NewAIClient returns nil when no CFOIL_AI_API_KEY is configured — the
// caller shows rules-only surfaces and says so. Nothing here pretends.
func NewAIClient(a *App) *AIClient {
	key := os.Getenv("CFOIL_AI_API_KEY")
	if key == "" {
		return nil
	}
	return &AIClient{
		app:      a,
		apiKey:   key,
		model:    envOr("CFOIL_AI_MODEL", "gemini-2.5-flash"),
		fallback: envOr("CFOIL_AI_MODEL_FALLBACK", "gemini-3.5-flash-lite"),
		endpoint: strings.TrimRight(envOr("CFOIL_GEMINI_URL", "https://generativelanguage.googleapis.com/v1beta"), "/"),
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (ai *AIClient) Model() string { return ai.model }

// ---------------------------------------------------------------- tools

// AIParam describes one argument of a tool. Types are the JSON ones the
// validator can actually check: string, number, boolean.
type AIParam struct {
	Type string
	Desc string
	Enum []string
}

// AITool is a typed, server-defined operation the model may request.
// Run executes real application code; its result — success or error —
// is what the model sees. Results are never fabricated: an error runs
// back into the loop as an error, marked so in the step log.
type AITool struct {
	Name     string
	Desc     string
	Params   map[string]AIParam
	Required []string
	Run      func(args map[string]any) (map[string]any, error)
}

// AgentStep is one line of the honest execution log: what actually ran,
// in order, with its outcome. The interface shows these — and only these.
type AgentStep struct {
	Kind   string // "model" | "tool"
	Name   string // model name, or tool name
	Detail string // compact, non-sensitive summary
	OK     bool
	Ms     int64
}

// validateArgs checks a model-proposed argument map against the tool's
// declared schema before anything runs: required present, no unknown
// names, JSON types right, enums respected. The error text goes back to
// the model as a tool error so it can correct itself.
func validateArgs(t AITool, args map[string]any) error {
	for _, req := range t.Required {
		if _, ok := args[req]; !ok {
			return fmt.Errorf("missing required argument %q", req)
		}
	}
	for k, v := range args {
		spec, ok := t.Params[k]
		if !ok {
			return fmt.Errorf("unknown argument %q", k)
		}
		switch spec.Type {
		case "string":
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("argument %q must be a string", k)
			}
			if len(spec.Enum) > 0 {
				found := false
				for _, e := range spec.Enum {
					if e == s {
						found = true
					}
				}
				if !found {
					return fmt.Errorf("argument %q must be one of %s", k, strings.Join(spec.Enum, ", "))
				}
			}
		case "number":
			if _, ok := v.(float64); !ok {
				return fmt.Errorf("argument %q must be a number", k)
			}
		case "boolean":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("argument %q must be a boolean", k)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- Gemini wire

type gemPart struct {
	Text             string     `json:"text,omitempty"`
	Thought          bool       `json:"thought,omitempty"`
	FunctionCall     *gemFnCall `json:"functionCall,omitempty"`
	FunctionResponse *gemFnResp `json:"functionResponse,omitempty"`
	// Newer Gemini models sign their reasoning into tool-call parts and
	// require the signature echoed back verbatim on the next turn.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type gemFnCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type gemFnResp struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type gemContent struct {
	Role  string    `json:"role"` // "user" | "model"
	Parts []gemPart `json:"parts"`
}

type gemRequest struct {
	Contents          []gemContent   `json:"contents"`
	SystemInstruction *gemContent    `json:"systemInstruction,omitempty"`
	Tools             []gemToolDecls `json:"tools,omitempty"`
	GenerationConfig  map[string]any `json:"generationConfig,omitempty"`
}

type gemToolDecls struct {
	FunctionDeclarations []map[string]any `json:"functionDeclarations"`
}

func declsOf(tools []AITool) []gemToolDecls {
	if len(tools) == 0 {
		return nil
	}
	var fns []map[string]any
	for _, t := range tools {
		props := map[string]any{}
		for name, p := range t.Params {
			prop := map[string]any{"type": strings.ToUpper(p.Type), "description": p.Desc}
			if len(p.Enum) > 0 {
				prop["enum"] = p.Enum
			}
			props[name] = prop
		}
		params := map[string]any{"type": "OBJECT", "properties": props}
		if len(t.Required) > 0 {
			params["required"] = t.Required
		}
		fns = append(fns, map[string]any{"name": t.Name, "description": t.Desc, "parameters": params})
	}
	return []gemToolDecls{{FunctionDeclarations: fns}}
}

type gemResult struct {
	parts  []gemPart
	tokens int64
	model  string // which model actually served (primary or fallback)
}

var errRetriable = errors.New("retriable")

// generate performs one generateContent call against the given model.
// The API key travels in a header, never in the URL, so it cannot leak
// through logs or referrers.
func (ai *AIClient) generate(model string, req gemRequest) (gemResult, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return gemResult{}, err
	}
	httpReq, err := http.NewRequest("POST", ai.endpoint+"/models/"+model+":generateContent", bytes.NewReader(b))
	if err != nil {
		return gemResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", ai.apiKey)
	resp, err := ai.client.Do(httpReq)
	if err != nil {
		return gemResult{}, fmt.Errorf("%w: %v", errRetriable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return gemResult{}, fmt.Errorf("%w: model API status %d", errRetriable, resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		// 4xx carries a reason worth surfacing (bad schema, blocked, …);
		// the body never contains our key.
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error.Message
		if len(msg) > 160 {
			msg = msg[:160]
		}
		return gemResult{}, fmt.Errorf("model API status %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Candidates []struct {
			Content      gemContent `json:"content"`
			FinishReason string     `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			TotalTokenCount int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return gemResult{}, err
	}
	if len(out.Candidates) == 0 {
		return gemResult{}, fmt.Errorf("model returned no candidates")
	}
	return gemResult{parts: out.Candidates[0].Content.Parts, tokens: out.UsageMetadata.TotalTokenCount, model: model}, nil
}

// callMetered tries the primary model, falls back once on rate limits,
// 5xx or transport errors, and receipts whichever call succeeded. Every
// model call in the product goes through here — the dashboard's spend
// figure is the sum of these receipts, not an estimate.
func (ai *AIClient) callMetered(purpose string, req gemRequest) (gemResult, error) {
	res, err := ai.generate(ai.model, req)
	if err != nil && errors.Is(err, errRetriable) && ai.fallback != "" && ai.fallback != ai.model {
		res, err = ai.generate(ai.fallback, req)
	}
	if err != nil {
		return gemResult{}, err
	}
	if ai.app != nil {
		_ = ai.app.emit(core.Receipt{
			Action: "model.call", ActorType: "agent", ActorID: "ai-service",
			Data: map[string]any{"provider": "gemini", "model": res.model,
				"tokens": float64(res.tokens), "purpose": purpose},
		})
	}
	return res, nil
}

// ---------------------------------------------------------------- agent loop

// maxToolFailures aborts a loop where the model keeps proposing invalid
// or failing calls instead of correcting course.
const maxToolFailures = 3

// AIMsg is one prior turn of context, provider-neutral.
type AIMsg struct {
	Role string // "user" | "model"
	Text string
}

// RunAgent is the real tool-calling loop: user request → model reasoning
// → tool selection → backend execution → result processing → final
// response. Tools execute serially, one at a time, in the order the
// model asks — never speculatively, never twice for one ask — and every
// execution lands in the step log exactly as it went.
func (ai *AIClient) RunAgent(purpose, system string, history []AIMsg, userMsg string, tools []AITool, maxIters int) (final string, steps []AgentStep, err error) {
	byName := map[string]AITool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	var contents []gemContent
	for _, m := range history {
		contents = append(contents, gemContent{Role: m.Role, Parts: []gemPart{{Text: m.Text}}})
	}
	contents = append(contents, gemContent{Role: "user", Parts: []gemPart{{Text: userMsg}}})
	req := gemRequest{
		SystemInstruction: &gemContent{Parts: []gemPart{{Text: system}}},
		Tools:             declsOf(tools),
	}
	failures := 0
	for iter := 0; iter < maxIters; iter++ {
		req.Contents = contents
		t0 := time.Now()
		res, err := ai.callMetered(purpose, req)
		if err != nil {
			steps = append(steps, AgentStep{Kind: "model", Name: ai.model, Detail: err.Error(), OK: false, Ms: time.Since(t0).Milliseconds()})
			return "", steps, err
		}
		steps = append(steps, AgentStep{Kind: "model", Name: res.model, OK: true, Ms: time.Since(t0).Milliseconds()})

		var calls []gemFnCall
		var text strings.Builder
		for _, p := range res.parts {
			if p.FunctionCall != nil {
				calls = append(calls, *p.FunctionCall)
			}
			if p.Text != "" && !p.Thought {
				text.WriteString(p.Text)
			}
		}
		if len(calls) == 0 {
			out := strings.TrimSpace(text.String())
			if out == "" {
				return "", steps, fmt.Errorf("model returned an empty answer")
			}
			return out, steps, nil
		}

		// Echo the model turn EXACTLY as received — newer models bind a
		// thought signature to their tool calls and refuse a stripped
		// echo — then execute each requested tool for real.
		contents = append(contents, gemContent{Role: "model", Parts: res.parts})
		var respParts []gemPart
		for _, call := range calls {
			result, ok := ai.execTool(byName, call, &steps)
			if !ok {
				failures++
				if failures >= maxToolFailures {
					return "", steps, fmt.Errorf("the model could not use the tools correctly (%d failed calls)", failures)
				}
			}
			respParts = append(respParts, gemPart{FunctionResponse: &gemFnResp{ID: call.ID, Name: call.Name, Response: result}})
		}
		contents = append(contents, gemContent{Role: "user", Parts: respParts})
	}
	return "", steps, fmt.Errorf("tool loop did not settle in %d turns", maxIters)
}

// execTool validates and runs one model-proposed call. The returned map
// is what the model sees; ok=false counts toward the failure budget.
func (ai *AIClient) execTool(byName map[string]AITool, call gemFnCall, steps *[]AgentStep) (map[string]any, bool) {
	t0 := time.Now()
	tool, known := byName[call.Name]
	if !known {
		*steps = append(*steps, AgentStep{Kind: "tool", Name: call.Name, Detail: "refused: no such tool", Ms: time.Since(t0).Milliseconds()})
		return map[string]any{"error": "no such tool"}, false
	}
	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	if err := validateArgs(tool, args); err != nil {
		*steps = append(*steps, AgentStep{Kind: "tool", Name: call.Name, Detail: "refused: " + err.Error(), Ms: time.Since(t0).Milliseconds()})
		return map[string]any{"error": err.Error()}, false
	}
	result, err := tool.Run(args)
	if err != nil {
		*steps = append(*steps, AgentStep{Kind: "tool", Name: call.Name, Detail: "failed: " + err.Error(), Ms: time.Since(t0).Milliseconds()})
		return map[string]any{"error": err.Error()}, false
	}
	*steps = append(*steps, AgentStep{Kind: "tool", Name: call.Name, Detail: summarize(args, result), OK: true, Ms: time.Since(t0).Milliseconds()})
	if result == nil {
		result = map[string]any{}
	}
	return result, true
}

// summarize renders a short, non-sensitive line for the step log.
func summarize(args, result map[string]any) string {
	var bits []string
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := fmt.Sprint(args[k])
		if len(v) > 40 {
			v = v[:40] + "…"
		}
		bits = append(bits, k+"="+v)
	}
	line := strings.Join(bits, " ")
	if s, ok := result["summary"].(string); ok && s != "" {
		if line != "" {
			line += " → "
		}
		line += s
		if len(line) > 90 {
			line = line[:90] + "…"
		}
	}
	return line
}

// GenerateText is the one-shot drafting call (no tools): used for the
// intake acknowledgement and other read-and-draft work.
func (ai *AIClient) GenerateText(purpose, system, user string) (string, error) {
	res, err := ai.callMetered(purpose, gemRequest{
		SystemInstruction: &gemContent{Parts: []gemPart{{Text: system}}},
		Contents:          []gemContent{{Role: "user", Parts: []gemPart{{Text: user}}}},
	})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, p := range res.parts {
		if !p.Thought {
			sb.WriteString(p.Text)
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("model returned an empty answer")
	}
	return out, nil
}

// GenerateJSON asks for schema-constrained JSON (Gemini's structured
// output) and returns the parsed object. Used where the answer must be
// typed — e.g. the in-doubt draft's recommendation enum.
func (ai *AIClient) GenerateJSON(purpose, system, user string, schema map[string]any) (map[string]any, error) {
	res, err := ai.callMetered(purpose, gemRequest{
		SystemInstruction: &gemContent{Parts: []gemPart{{Text: system}}},
		Contents:          []gemContent{{Role: "user", Parts: []gemPart{{Text: user}}}},
		GenerationConfig: map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   schema,
		},
	})
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	for _, p := range res.parts {
		if !p.Thought {
			sb.WriteString(p.Text)
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(sb.String()), &out); err != nil {
		return nil, fmt.Errorf("model output was not the requested JSON: %v", err)
	}
	return out, nil
}

// HistoryFromChat converts recent chat into model turns (text only;
// cards and system notes stay out). The last few messages are enough
// context without shipping the whole transcript on every send.
func HistoryFromChat(msgs []ChatMsg, limit int) []AIMsg {
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	var out []AIMsg
	for _, m := range msgs {
		role := "user"
		if m.From == "counterfoil" {
			role = "model"
		}
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		out = append(out, AIMsg{Role: role, Text: m.Text})
	}
	// Gemini requires the first turn to be a user turn.
	for len(out) > 0 && out[0].Role != "user" {
		out = out[1:]
	}
	return out
}
