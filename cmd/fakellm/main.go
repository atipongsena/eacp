// Command fakellm is the development LLM provider of the LLM gateway demo
// (ADR-031): Anthropic Messages and OpenAI Chat Completions with fixed
// answers and exact usage, behind its own key, with a durable audit log.
package main

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"eacp/internal/config"
	"eacp/internal/fakellm"
	"eacp/internal/service"
)

func loadHandler(keyPath, dataPath string) (http.Handler, string, error) {
	if keyPath == "" || dataPath == "" {
		return nil, "", errors.New("EACP_FAKELLM_KEY_FILE and EACP_FAKELLM_DATA_FILE are required")
	}
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, "", errors.New("fakellm: cannot read the key file")
	}
	key := strings.TrimRight(string(b), "\r\n")
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, " \t\r\n") {
		return nil, "", errors.New("fakellm: invalid key file")
	}
	h, err := fakellm.New(key, dataPath)
	if err != nil {
		return nil, "", err
	}
	return h, key, nil
}

func main() {
	service.Main("fakellm", config.Options{RequireDatabase: false, DefaultHTTPAddr: ":8093"},
		func(d *service.Deps, mux *http.ServeMux) error {
			h, key, err := loadHandler(os.Getenv("EACP_FAKELLM_KEY_FILE"), os.Getenv("EACP_FAKELLM_DATA_FILE"))
			if err != nil {
				return err
			}
			d.RedactSecrets(key)
			mux.Handle("/", h)
			return nil
		})
}
