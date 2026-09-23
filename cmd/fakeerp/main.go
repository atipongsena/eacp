// Command fakeerp is the credential-protected Slice A demo target. It keeps
// operation and audit evidence in a durable append log for lookup.
package main

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"eacp/internal/config"
	"eacp/internal/fakeerp"
	"eacp/internal/service"
)

func loadHandler(tokenPath, dataPath string) (http.Handler, string, error) {
	if tokenPath == "" || dataPath == "" {
		return nil, "", errors.New("EACP_FAKEERP_TOKEN_FILE and EACP_FAKEERP_DATA_FILE are required")
	}
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, "", errors.New("fakeerp: cannot read credential file")
	}
	token := strings.TrimRight(string(b), "\r\n")
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return nil, "", errors.New("fakeerp: invalid credential file")
	}
	h, err := fakeerp.New(token, dataPath)
	if err != nil {
		return nil, "", err
	}
	return h, token, nil
}

func main() {
	service.Main("fakeerp", config.Options{RequireDatabase: false, DefaultHTTPAddr: ":8090"},
		func(d *service.Deps, mux *http.ServeMux) error {
			h, token, err := loadHandler(os.Getenv("EACP_FAKEERP_TOKEN_FILE"), os.Getenv("EACP_FAKEERP_DATA_FILE"))
			if err != nil {
				return err
			}
			d.RedactSecrets(token)
			mux.Handle("/", h)
			return nil
		})
}
