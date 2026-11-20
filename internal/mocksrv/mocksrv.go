// Package mocksrv is the target stand-ins: the other teams' systems that
// Counterfoil acts on — an IT service desk, a directory, an access-card
// system and a payroll system. They are ours, labelled ours, and they
// exist so a judge can see the *target's own records* beside Counterfoil's
// claims. Each service has a lie mode (reply success, store nothing) and
// honours Idempotency-Key where the real world plausibly would.
//
// TBO's own mock APIs are not published; when they are, Counterfoil's
// dispatcher points at them instead and this process stays for drills.
package mocksrv

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Record map[string]any

type Service struct {
	Name       string
	Collection string
	Lie        bool
	Seen       map[string]string // Idempotency-Key -> record id (honoured)
	Records    map[string]Record
	Counter    int
}

type Server struct {
	mu       sync.Mutex
	dir      string
	services map[string]*Service
	mux      *http.ServeMux
}

func New(dataDir string, employeesPath string) (*Server, error) {
	s := &Server{dir: dataDir, services: map[string]*Service{}, mux: http.NewServeMux()}
	for _, name := range []string{"itsm", "directory", "cards", "payroll", "canarysys"} {
		s.services[name] = &Service{Name: name, Collection: collectionOf(name),
			Seen: map[string]string{}, Records: map[string]Record{}}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if !s.load() {
		if err := s.seed(employeesPath); err != nil {
			return nil, err
		}
	}
	s.routes()
	return s, nil
}

func collectionOf(name string) string {
	switch name {
	case "itsm":
		return "tickets"
	case "directory":
		return "users"
	case "cards":
		return "cards"
	case "payroll":
		return "payments"
	default:
		return "items"
	}
}

func (s *Server) statePath() string { return filepath.Join(s.dir, "mocks_state.json") }

func (s *Server) load() bool {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		return false
	}
	var snap map[string]*Service
	if json.Unmarshal(b, &snap) != nil {
		return false
	}
	for k, v := range snap {
		if v.Seen == nil {
			v.Seen = map[string]string{}
		}
		if v.Records == nil {
			v.Records = map[string]Record{}
		}
		s.services[k] = v
	}
	return true
}

func (s *Server) save() {
	b, _ := json.MarshalIndent(s.services, "", " ")
	_ = os.WriteFile(s.statePath(), b, 0o644)
}

func (s *Server) seed(employeesPath string) error {
	type emp struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Email  string   `json:"email"`
		CardID string   `json:"card_id"`
		Assets []string `json:"assets"`
	}
	b, err := os.ReadFile(employeesPath)
	if err != nil {
		return err
	}
	var emps []emp
	if err := json.Unmarshal(b, &emps); err != nil {
		return err
	}
	dir := s.services["directory"]
	cards := s.services["cards"]
	for _, e := range emps {
		dir.Records[e.Email] = Record{"id": e.Email, "email": e.Email, "name": e.Name,
			"employee_id": e.ID, "status": "active", "updated": time.Now().UTC()}
		if e.CardID != "" {
			cards.Records[e.CardID] = Record{"id": e.CardID, "card_id": e.CardID,
				"employee_id": e.ID, "status": "active", "zones": "general, cafeteria",
				"updated": time.Now().UTC()}
		}
	}
	s.save()
	return nil
}

// ---------------------------------------------------------------- HTTP

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.ui)
	s.mux.HandleFunc("POST /admin/{svc}/lie", s.setLie)
	s.mux.HandleFunc("POST /admin/reset", s.reset)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })

	// Writes.
	s.mux.HandleFunc("POST /itsm/tickets", s.create("itsm", "TKT"))
	s.mux.HandleFunc("POST /payroll/stage", s.create("payroll", "DRF"))
	s.mux.HandleFunc("POST /payroll/payments", s.create("payroll", "PMT"))
	s.mux.HandleFunc("POST /canarysys/items", s.create("canarysys", "CNY"))
	s.mux.HandleFunc("POST /directory/disable", s.setStatus("directory", "email", "disabled"))
	s.mux.HandleFunc("POST /cards/revoke", s.setStatus("cards", "card_id", "revoked"))

	// Reads: the other team's own records.
	for _, svc := range []string{"itsm", "directory", "cards", "payroll", "canarysys"} {
		name := svc
		col := collectionOf(svc)
		s.mux.HandleFunc(fmt.Sprintf("GET /%s/%s", name, col), func(w http.ResponseWriter, r *http.Request) {
			s.list(name, w, r)
		})
		s.mux.HandleFunc(fmt.Sprintf("GET /%s/%s/{id}", name, col), func(w http.ResponseWriter, r *http.Request) {
			s.get(name, w, r)
		})
	}
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// create handles POSTs that make a new record (tickets, payments, drafts).
func (s *Server) create(svcName, prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := r.Header.Get("Idempotency-Key")
		ref := r.Header.Get("X-Client-Ref")
		s.mu.Lock()
		defer s.mu.Unlock()
		svc := s.services[svcName]
		// Idempotency: the same key returns the same record, never two.
		if key != "" {
			if id, ok := svc.Seen[key]; ok {
				jsonOut(w, 200, svc.Records[id])
				return
			}
		}
		svc.Counter++
		id := fmt.Sprintf("%s-%04d", prefix, svc.Counter)
		rec := Record{"id": id, "client_ref": ref, "created": time.Now().UTC(), "status": "open"}
		for k, v := range body {
			rec[k] = v
		}
		if svcName == "payroll" {
			if strings.HasPrefix(id, "PMT") {
				rec["status"] = "released"
			} else {
				rec["status"] = "held"
			}
		}
		if svc.Lie || svcName == "canarysys" {
			// The lie: reply success, store nothing. The reply is a claim;
			// the record is the fact — and there is no record.
			jsonOut(w, 201, Record{"id": id, "status": rec["status"]})
			return
		}
		svc.Records[id] = rec
		if key != "" {
			svc.Seen[key] = id
		}
		s.save()
		jsonOut(w, 201, rec)
	}
}

// setStatus handles convergent state changes (disable, revoke).
func (s *Server) setStatus(svcName, idField, status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body[idField].(string)
		ref := r.Header.Get("X-Client-Ref")
		s.mu.Lock()
		defer s.mu.Unlock()
		svc := s.services[svcName]
		rec, ok := svc.Records[id]
		if !ok {
			jsonOut(w, 404, Record{"error": "no such record", idField: id})
			return
		}
		if svc.Lie {
			jsonOut(w, 200, Record{"id": id, "status": status})
			return
		}
		rec["status"] = status
		rec["last_action_ref"] = ref
		rec["client_ref"] = ref
		rec["updated"] = time.Now().UTC()
		s.save()
		jsonOut(w, 200, rec)
	}
}

func (s *Server) list(svcName string, w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("client_ref")
	s.mu.Lock()
	defer s.mu.Unlock()
	svc := s.services[svcName]
	out := []Record{}
	for _, rec := range svc.Records {
		if ref != "" {
			if rec["client_ref"] != ref && rec["last_action_ref"] != ref {
				continue
			}
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i]["id"].(string)
		b, _ := out[j]["id"].(string)
		return a < b
	})
	jsonOut(w, 200, out)
}

func (s *Server) get(svcName string, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	svc := s.services[svcName]
	if rec, ok := svc.Records[id]; ok {
		jsonOut(w, 200, rec)
		return
	}
	jsonOut(w, 404, Record{"error": "not found"})
}

func (s *Server) setLie(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("svc")
	on := r.URL.Query().Get("on") == "1" || r.FormValue("on") == "1"
	s.mu.Lock()
	if svc, ok := s.services[name]; ok {
		svc.Lie = on
		s.save()
	}
	s.mu.Unlock()
	if r.Header.Get("Accept") == "application/json" {
		jsonOut(w, 200, Record{"service": name, "lie": on})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	for _, svc := range s.services {
		svc.Lie = false
	}
	s.save()
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------------------------------------------------------------- tiny UI

var uiTmpl = template.Must(template.New("ui").Parse(`<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>TargetSys — stand-in systems</title>
<style>
:root{--ink:#1d2730;--muted:#5d6a76;--hair:#d4dade;--mist:#f2f4f6;--amber:#8a5a00;--red:#b42318}
body{font:14px/1.45 "Segoe UI",system-ui,sans-serif;color:var(--ink);margin:0;background:#fafbfc}
header{background:#243240;color:#fff;padding:14px 28px}
header h1{font-size:16px;margin:0;font-weight:600}
header p{margin:2px 0 0;color:#b8c4cf;font-size:12.5px}
main{max-width:1060px;margin:22px auto;padding:0 20px}
section{background:#fff;border:1px solid var(--hair);border-radius:6px;margin:0 0 18px;overflow:hidden}
section>h2{font-size:13.5px;margin:0;padding:10px 16px;border-bottom:1px solid var(--hair);display:flex;justify-content:space-between;align-items:center}
table{width:100%;border-collapse:collapse;font-size:12.5px}
td,th{padding:6px 16px;border-bottom:1px solid var(--mist);text-align:left;vertical-align:top}
th{color:var(--muted);font-weight:600;background:var(--mist)}
.lie{color:var(--red);font-weight:600}
.ok{color:#1e7a46}
button{font:12.5px "Segoe UI";padding:4px 10px;border:1px solid var(--hair);background:#fff;border-radius:4px;cursor:pointer}
.pill{font-size:11px;border:1px solid var(--hair);border-radius:99px;padding:1px 8px;color:var(--muted)}
</style>
<header>
 <h1>TargetSys — the other teams' systems (stand-ins)</h1>
 <p>Built and labelled by Team Init to Win It. These are not TBO's mocks. What is written here is the fact Counterfoil checks against.</p>
</header>
<main>
{{range .}}
<section>
 <h2>{{.Title}}
  <span>
   {{if .Lie}}<span class="lie">LIE MODE: replies success, stores nothing</span>{{else}}<span class="ok">honest</span>{{end}}
   <form method="post" action="/admin/{{.Name}}/lie" style="display:inline">
     <input type="hidden" name="on" value="{{if .Lie}}0{{else}}1{{end}}">
     <button>{{if .Lie}}Turn lie mode off{{else}}Turn lie mode on{{end}}</button>
   </form>
  </span>
 </h2>
 <table>
 <tr><th style="width:110px">ID</th><th>Record</th><th style="width:90px">Status</th><th style="width:150px">client_ref</th></tr>
 {{range .Rows}}<tr><td>{{.ID}}</td><td>{{.Body}}</td><td>{{.Status}}</td><td><span class="pill">{{.Ref}}</span></td></tr>
 {{else}}<tr><td colspan="4" style="color:#5d6a76">no records</td></tr>{{end}}
 </table>
</section>
{{end}}
<form method="post" action="/admin/reset"><button>Turn every lie mode off</button></form>
<p style="color:#5d6a76;font-size:12px">A record created while lie mode is on simply does not exist here — that is the point.</p>
</main>`))

type uiRow struct{ ID, Body, Status, Ref string }
type uiSvc struct {
	Name, Title string
	Lie         bool
	Rows        []uiRow
}

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	titles := map[string]string{
		"itsm": "IT Service Desk — tickets", "directory": "Directory — accounts",
		"cards": "Access cards", "payroll": "Payroll — drafts and payments",
		"canarysys": "Canary target (always lies, by design)"}
	var out []uiSvc
	for _, name := range []string{"directory", "cards", "itsm", "payroll", "canarysys"} {
		svc := s.services[name]
		u := uiSvc{Name: name, Title: titles[name], Lie: svc.Lie || name == "canarysys"}
		var ids []string
		for id := range svc.Records {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rec := svc.Records[id]
			var parts []string
			for _, k := range []string{"name", "email", "employee_id", "type", "asset", "amount_inr", "period", "zones", "due"} {
				if v, ok := rec[k]; ok && v != "" && v != nil {
					parts = append(parts, fmt.Sprintf("%s: %v", k, v))
				}
			}
			ref, _ := rec["client_ref"].(string)
			if ref == "" {
				ref, _ = rec["last_action_ref"].(string)
			}
			st, _ := rec["status"].(string)
			u.Rows = append(u.Rows, uiRow{ID: id, Body: strings.Join(parts, " · "), Status: st, Ref: ref})
		}
		out = append(out, u)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = uiTmpl.Execute(w, out)
}
