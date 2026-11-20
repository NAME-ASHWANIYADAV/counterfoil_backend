// cfoil is Counterfoil's binary. One executable, several roles:
//
//	cfoil demo              start signer + stand-ins + the product (the judge's command)
//	cfoil serve             the product alone
//	cfoil signer            the separate signing process
//	cfoil mocks             the target stand-ins
//	cfoil check <dir>       the package admission check
//	cfoil verify -dir <d>   the offline audit verifier
//
// cmd/cfoil-signer, cmd/cfoil-mocks and cmd/verify-audit are thin wrappers
// over the same roles, for running them as visibly separate binaries.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"counterfoil/internal/audit"
	"counterfoil/internal/core"
	"counterfoil/internal/kernel"
	"counterfoil/internal/mocksrv"
	"counterfoil/internal/signersrv"
	"counterfoil/internal/webapp"
)

func main() {
	// Secrets and model configuration come from the environment; a .env
	// at the repo root fills what is not already set (and never
	// overwrites what is). Values are never printed. Child processes
	// spawned by `demo` inherit the result.
	core.LoadDotEnv(filepath.Join(repoRoot(), ".env"))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serveMain(args)
	case "demo":
		err = demoMain(args)
	case "signer":
		err = SignerMain(args)
	case "mocks":
		err = MocksMain(args)
	case "check":
		err = checkMain(args)
	case "verify":
		err = VerifyMain(args)
	case "version":
		fmt.Println("counterfoil 0.1.0 (prototype)")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cfoil:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  cfoil demo    [-reset]        one command: signer + stand-ins + product
  cfoil serve   [flags]         the product alone
  cfoil signer  [-listen :7071] the separate signing process
  cfoil mocks   [-listen :7072] the target stand-ins
  cfoil check   <package dir>   admission check for a service package
  cfoil verify  -dir export     offline audit verifier`)
}

func repoRoot() string {
	// The repo root holds extensions/, policy/, rules/. Walk up from the
	// working directory until they are found (so `go run ./cmd/cfoil`
	// works from the root and from subdirectories).
	dir, _ := os.Getwd()
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "extensions")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	d, _ := os.Getwd()
	return d
}

func defaultCfg(root, dataRoot string) kernel.Config {
	return kernel.Config{
		DataDir:       filepath.Join(dataRoot, "cfoil"),
		MocksURL:      "http://127.0.0.1:7072",
		SignerURL:     "http://127.0.0.1:7071",
		ExtDir:        filepath.Join(root, "extensions"),
		PolicyPath:    filepath.Join(root, "policy", "reserved.json"),
		RulesPath:     filepath.Join(root, "rules", "rulepack.json"),
		UsersPath:     filepath.Join(root, "rules", "users.json"),
		EmployeesPath: filepath.Join(root, "rules", "employees.json"),
	}
}

func serveMain(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7070", "listen address")
	dataRoot := fs.String("data", "", "data directory (default <repo>/data)")
	mocks := fs.String("mocks", "http://127.0.0.1:7072", "target stand-ins base URL")
	signer := fs.String("signer", "http://127.0.0.1:7071", "signer base URL")
	_ = fs.Parse(args)
	// Hosted platforms (Render, Railway, …) inject PORT and route external
	// traffic to it; honour it unless -listen was set explicitly.
	listenSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "listen" {
			listenSet = true
		}
	})
	if !listenSet {
		if p := os.Getenv("PORT"); p != "" {
			*listen = "0.0.0.0:" + p
		}
	}
	if h, _, err := net.SplitHostPort(*listen); err == nil &&
		h != "127.0.0.1" && h != "localhost" && h != "::1" && h != "" {
		log.Printf("serve: binding %s — publicly reachable. This demo signs in by persona, without passwords, over synthetic data by design; set CFOIL_SIGNER_TOKEN in any shared deployment.", *listen)
	}
	root := repoRoot()
	if *dataRoot == "" {
		*dataRoot = filepath.Join(root, "data")
	}
	cfg := defaultCfg(root, *dataRoot)
	cfg.MocksURL, cfg.SignerURL = *mocks, *signer
	app, err := kernel.New(cfg)
	if err != nil {
		return err
	}
	app.Start()
	web, err := webapp.New(app)
	if err != nil {
		return err
	}
	fmt.Printf("cfoil serve: http://%s  (data %s)\n", *listen, cfg.DataDir)
	startKeepalive()
	return http.ListenAndServe(*listen, web.Handler())
}

// startKeepalive pings the service's own public URL so a free-tier host
// (for example Render) never idles it to sleep. It is off unless
// CFOIL_KEEPALIVE_URL — or the RENDER_EXTERNAL_URL that Render injects —
// is set, so local runs are untouched. The ping must go through the
// host's public edge: idle detection counts proxied requests, so hitting
// localhost would not keep the instance awake.
func startKeepalive() {
	base := os.Getenv("CFOIL_KEEPALIVE_URL")
	if base == "" {
		base = os.Getenv("RENDER_EXTERNAL_URL")
	}
	if base == "" {
		return
	}
	url := strings.TrimRight(base, "/") + "/healthz"
	every := 60 * time.Second
	if s := os.Getenv("CFOIL_KEEPALIVE_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 20 {
			every = time.Duration(n) * time.Second
		}
	}
	log.Printf("keepalive: pinging %s every %s", url, every)
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		fails, announced := 0, false
		for {
			time.Sleep(every)
			resp, err := client.Get(url)
			if err != nil {
				fails++
				if fails <= 3 || fails%30 == 0 {
					log.Printf("keepalive: ping failed (%d in a row): %v", fails, err)
				}
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if !announced {
				log.Printf("keepalive: first ping ok (%s)", resp.Status)
				announced = true
			}
			fails = 0
		}
	}()
}

func SignerMain(args []string) error {
	fs := flag.NewFlagSet("signer", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7071", "listen address")
	dataRoot := fs.String("data", "", "data directory (default <repo>/data)")
	_ = fs.Parse(args)
	root := repoRoot()
	if *dataRoot == "" {
		*dataRoot = filepath.Join(root, "data")
	}
	s, err := signersrv.New(filepath.Join(*dataRoot, "signer"))
	if err != nil {
		return err
	}
	fmt.Printf("cfoil-signer: http://%s  (keys stay in this process)\n", *listen)
	return http.ListenAndServe(*listen, s.Handler())
}

func MocksMain(args []string) error {
	fs := flag.NewFlagSet("mocks", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7072", "listen address")
	dataRoot := fs.String("data", "", "data directory (default <repo>/data)")
	_ = fs.Parse(args)
	root := repoRoot()
	if *dataRoot == "" {
		*dataRoot = filepath.Join(root, "data")
	}
	s, err := mocksrv.New(filepath.Join(*dataRoot, "mocks"), filepath.Join(root, "rules", "employees.json"))
	if err != nil {
		return err
	}
	fmt.Printf("cfoil-mocks: http://%s  (the other teams' systems, stand-ins)\n", *listen)
	return http.ListenAndServe(*listen, s.Handler())
}

func demoMain(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	reset := fs.Bool("reset", false, "wipe the data directory first (fresh demo)")
	_ = fs.Parse(args)
	root := repoRoot()
	dataRoot := filepath.Join(root, "data")
	if *reset {
		if err := os.RemoveAll(dataRoot); err != nil {
			return err
		}
		fmt.Println("demo: data wiped for a fresh run")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// The signer and the stand-ins run as genuinely separate processes.
	for _, role := range []string{"signer", "mocks"} {
		c := exec.Command(self, role)
		c.Dir = root
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Start(); err != nil {
			return fmt.Errorf("start %s: %w", role, err)
		}
		defer func(p *os.Process) { _ = p.Kill() }(c.Process)
	}
	for _, u := range []string{"http://127.0.0.1:7071/healthz", "http://127.0.0.1:7072/healthz"} {
		if err := waitUp(u, 6*time.Second); err != nil {
			return err
		}
	}
	fmt.Println("demo: signer and stand-ins are up")
	port := "7070"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	fmt.Printf("demo: product  http://localhost:%s   stand-ins  http://localhost:7072\n", port)
	return serveMain(nil)
}

func waitUp(url string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("%s did not come up", url)
}

func checkMain(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cfoil check <package dir>")
	}
	root := repoRoot()
	m, err := kernel.LoadManifest(args[0])
	if err != nil {
		return err
	}
	floor, err := kernel.LoadReserved(filepath.Join(root, "policy", "reserved.json"))
	if err != nil {
		return err
	}
	res := kernel.CheckManifest(m, func(action string) (kernel.ReservedDef, bool) {
		for _, r := range floor {
			if r.Action == action && r.Action != "" {
				return r, true
			}
		}
		return kernel.ReservedDef{}, false
	})
	fmt.Printf("cfoil check — package %q v%s (%d steps)\n", m.Name, m.Version, len(m.Steps))
	fmt.Println("stages: schema + lint (crash test, Cedar and SymCC stages: design, not built)")
	for _, f := range res.Findings {
		tag := "REFUSED"
		if f.Severity == "warning" {
			tag = "warn   "
		}
		fmt.Printf("  %s  [%s] %s: %s\n", tag, f.Rule, f.Where, f.Message)
	}
	if res.Errors > 0 {
		fmt.Printf("RESULT: REFUSED — %d error(s), %d warning(s). Not certifiable; nothing is loaded.\n", res.Errors, res.Warnings)
		os.Exit(1)
	}
	fmt.Printf("RESULT: PASS — 0 errors, %d warning(s). Certificate sha256:%s…\n", res.Warnings, core.Short(res.Cert))
	fmt.Println("        (inactive until two named people activate exactly this hash)")
	return nil
}

func VerifyMain(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", "export", "bundle directory (receipts.jsonl + checkpoints.jsonl)")
	pub := fs.String("pub", "", "pin the signer public key (hex); overrides signer_pub.txt")
	_ = fs.Parse(args)
	checks, err := audit.Run(*dir, *pub)
	if err != nil {
		return err
	}
	pass := 0
	fmt.Printf("verify-audit — offline checks on %s (no network, no database)\n", *dir)
	for _, c := range checks {
		tag := "FAIL"
		if c.Pass {
			tag, pass = "pass", pass+1
		}
		fmt.Printf("  %2d %s  %s\n       %s\n", c.N, tag, c.Title, c.Detail)
	}
	fmt.Printf("RESULT: %d of %d checks pass\n", pass, len(checks))
	if pass != len(checks) {
		os.Exit(1)
	}
	return nil
}
