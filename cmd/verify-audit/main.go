// verify-audit re-checks an exported receipt bundle offline: the hash
// chain, the signer's countersignatures, mandates on reserved effects,
// read-backs behind every Done, clock closures, intent uniqueness,
// canaries and package activations. No network, no database.
package main

import (
	"flag"
	"fmt"
	"os"

	"counterfoil/internal/audit"
)

func main() {
	dir := flag.String("dir", "export", "bundle directory (receipts.jsonl + checkpoints.jsonl)")
	flag.Parse()
	checks, err := audit.Run(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-audit:", err)
		os.Exit(1)
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
}
