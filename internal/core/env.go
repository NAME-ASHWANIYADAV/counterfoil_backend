package core

import (
	"os"
	"strings"
)

// LoadDotEnv reads KEY=VALUE lines from path into the process environment.
// A variable that is already set in the environment is never overwritten —
// the file fills gaps, it does not replace configuration. Values are never
// logged or printed anywhere. A missing file is not an error: the product
// runs whole without one.
func LoadDotEnv(path string) (loaded int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Strip one pair of surrounding quotes, if present.
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k == "" || v == "" {
			continue
		}
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		if os.Setenv(k, v) == nil {
			loaded++
		}
	}
	return loaded
}
