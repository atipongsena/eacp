// Command fakemcp is the credential-protected MCP server of the Slice C
// demo (internal/fakemcp). Its tool list is re-read from
// EACP_FAKEMCP_TOOLS_FILE on every listing, so the demo can make it drift.
package main

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"eacp/internal/config"
	"eacp/internal/fakemcp"
	"eacp/internal/service"
)

func loadHandler(tokenPath, toolsFile string) (http.Handler, string, error) {
	if tokenPath == "" {
		return nil, "", errors.New("EACP_FAKEMCP_TOKEN_FILE is required")
	}
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, "", errors.New("fakemcp: cannot read credential file")
	}
	token := strings.TrimRight(string(b), "\r\n")
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return nil, "", errors.New("fakemcp: invalid credential file")
	}
	h, err := fakemcp.New(token, toolsFile)
	if err != nil {
		return nil, "", err
	}
	return h, token, nil
}

func main() {
	service.Main("fakemcp", config.Options{RequireDatabase: false, DefaultHTTPAddr: ":8091"},
		func(d *service.Deps, mux *http.ServeMux) error {
			h, token, err := loadHandler(os.Getenv("EACP_FAKEMCP_TOKEN_FILE"), os.Getenv("EACP_FAKEMCP_TOOLS_FILE"))
			if err != nil {
				return err
			}
			d.RedactSecrets(token)
			mux.Handle("/mcp", h)
			return nil
		})
}
