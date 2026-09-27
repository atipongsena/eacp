// Command fakea2a is the credential-protected A2A agent of the A2A demo
// (internal/fakea2a, ADR-030). Its card is re-read from
// EACP_FAKEA2A_CARD_FILE on every request, so the demo can make it drift;
// without one it serves a built-in card naming EACP_FAKEA2A_ENDPOINT.
package main

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"

	"eacp/internal/config"
	"eacp/internal/fakea2a"
	"eacp/internal/service"
)

func loadHandler(getenv func(string) string) (http.Handler, string, error) {
	tokenPath, dataPath, endpoint := getenv("EACP_FAKEA2A_TOKEN_FILE"), getenv("EACP_FAKEA2A_DATA_FILE"),
		getenv("EACP_FAKEA2A_ENDPOINT")
	if tokenPath == "" || dataPath == "" || endpoint == "" {
		return nil, "", errors.New("EACP_FAKEA2A_TOKEN_FILE, EACP_FAKEA2A_DATA_FILE and EACP_FAKEA2A_ENDPOINT are required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Path != fakea2a.Path || u.RawQuery != "" || u.Fragment != "" {
		return nil, "", errors.New("fakea2a: EACP_FAKEA2A_ENDPOINT must be an http(s) URL ending in " + fakea2a.Path)
	}
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, "", errors.New("fakea2a: cannot read credential file")
	}
	token := strings.TrimRight(string(b), "\r\n")
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return nil, "", errors.New("fakea2a: invalid credential file")
	}
	h, err := fakea2a.New(token, getenv("EACP_FAKEA2A_CARD_FILE"), dataPath, endpoint)
	if err != nil {
		return nil, "", err
	}
	return h, token, nil
}

func main() {
	service.Main("fakea2a", config.Options{RequireDatabase: false, DefaultHTTPAddr: ":8092"},
		func(d *service.Deps, mux *http.ServeMux) error {
			h, token, err := loadHandler(os.Getenv)
			if err != nil {
				return err
			}
			d.RedactSecrets(token)
			mux.Handle("GET /.well-known/agent-card.json", h)
			mux.Handle("POST "+fakea2a.Path, h)
			mux.Handle("GET /v1/audit", h)
			return nil
		})
}
