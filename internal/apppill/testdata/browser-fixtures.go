package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/shaul/mesh/internal/apppill"
	"github.com/shaul/mesh/internal/testdomains"
)

func main() {
	if err := fixtures(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fixtures() error {
	cleanup := testdomains.Setup()
	defer cleanup()
	large := strings.Repeat("a", 300<<10)
	documents := map[string]string{
		"hoisted-csp": `<html><head><title>app</title></head><meta http-equiv="Content-Security-Policy" content="default-src 'self'"><body>app</body></html>`,
		"script":      "<html><script>window.appData='" + large + "';window.appOK=true;</script><body>app</body></html>",
		"style":       "<html><style>/*" + large + "*/body{color:rgb(1,2,3)}</style><body>app</body></html>",
		"textarea":    "<html><textarea>" + large + "</textarea><body>app</body></html>",
		"title":       "<html><title>" + large + "</title><body>app</body></html>",
	}
	for name, document := range documents {
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(document))}
		if err := apppill.Inject(resp, apppill.Config{AppID: "zzzz", ManagementOrigin: os.Args[1]}); err != nil {
			return fmt.Errorf("fixture %s: %w", name, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("fixture %s body: %w", name, err)
		}
		documents[name] = string(body)
	}
	if err := json.NewEncoder(os.Stdout).Encode(documents); err != nil {
		return fmt.Errorf("encode fixtures: %w", err)
	}
	return nil
}
