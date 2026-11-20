// cfoil-mocks: the target stand-ins (IT service desk, directory, access
// cards, payroll). See internal/mocksrv.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"counterfoil/internal/mocksrv"
)

func main() {
	listen := flag.String("listen", ":7072", "listen address")
	data := flag.String("data", "data", "data directory")
	emps := flag.String("employees", filepath.Join("rules", "employees.json"), "seed employees")
	flag.Parse()
	s, err := mocksrv.New(filepath.Join(*data, "mocks"), *emps)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cfoil-mocks:", err)
		os.Exit(1)
	}
	fmt.Printf("cfoil-mocks: http://localhost%s\n", *listen)
	if err := http.ListenAndServe(*listen, s.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "cfoil-mocks:", err)
		os.Exit(1)
	}
}
