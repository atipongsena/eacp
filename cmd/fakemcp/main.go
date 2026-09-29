// Command fakemcp is the credential-protected MCP server of the Slice C
// demo (internal/fakemcp). Its tool list is re-read from
// EACP_FAKEMCP_TOOLS_FILE on every listing, so the demo can make it drift;
// EACP_FAKEMCP_CALLS_FILE is its durable, content-free call log.
package main

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/atipongsena/eacp/internal/config"
	"github.com/atipongsena/eacp/internal/fakemcp"
	"github.com/atipongsena/eacp/internal/service"
)

func loadHandler(tokenPath, toolsFile, callsFile string) (http.Handler, string, error) {
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
	if callsFile != "" {
		if err := h.LogCallsTo(callsFile); err != nil {
			return nil, "", err
		}
	}
	return h, token, nil
}

func main() {
	service.Main("fakemcp", config.Options{RequireDatabase: false, DefaultHTTPAddr: ":8091"},
		func(d *service.Deps, mux *http.ServeMux) error {
			h, token, err := loadHandler(os.Getenv("EACP_FAKEMCP_TOKEN_FILE"), os.Getenv("EACP_FAKEMCP_TOOLS_FILE"), os.Getenv("EACP_FAKEMCP_CALLS_FILE"))
			if err != nil {
				return err
			}
			d.RedactSecrets(token)
			mux.Handle(fakemcp.Path, h)
			mux.Handle(fakemcp.CallsPath, h)
			return nil
		})
}
