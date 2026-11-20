// cfoil-signer: the separate process that alone holds the checkpoint
// signing key. See internal/signersrv.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"counterfoil/internal/signersrv"
)

func main() {
	listen := flag.String("listen", ":7071", "listen address")
	data := flag.String("data", "data", "data directory")
	flag.Parse()
	s, err := signersrv.New(filepath.Join(*data, "signer"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cfoil-signer:", err)
		os.Exit(1)
	}
	fmt.Printf("cfoil-signer: http://localhost%s\n", *listen)
	if err := http.ListenAndServe(*listen, s.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "cfoil-signer:", err)
		os.Exit(1)
	}
}
