// Command fakeerp is the credential-protected Slice A demo target. It keeps
// operation and audit evidence in a durable append log for lookup.
package main

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"eacp/internal/config"
	"eacp/internal/fakeerp"
	"eacp/internal/service"
)

func loadHandler(tokenPath, dataPath string, o fakeerp.Options) (http.Handler, string, error) {
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
	h, err := fakeerp.NewWithOptions(token, dataPath, o)
	if err != nil {
		return nil, "", err
	}
	return h, token, nil
}

// loadOAuth reads the optional OAuth client of the token endpoint. The id
// and the secret file come together; the TTL (default 300s, 1s-1h) applies
// to every issued token, federated ones too.
func loadOAuth(getenv func(string) string) (fakeerp.Options, error) {
	ttl := 300 * time.Second
	if v := getenv("EACP_FAKEERP_OAUTH_TTL"); v != "" {
		var err error
		if ttl, err = time.ParseDuration(v); err != nil || ttl < time.Second || ttl > time.Hour {
			return fakeerp.Options{}, errors.New("fakeerp: EACP_FAKEERP_OAUTH_TTL must be a duration from 1s to 1h")
		}
	}
	id, file := getenv("EACP_FAKEERP_OAUTH_CLIENT_ID"), getenv("EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE")
	if id == "" && file == "" {
		return fakeerp.Options{TokenTTL: ttl}, nil
	}
	if id == "" || file == "" {
		return fakeerp.Options{}, errors.New("fakeerp: EACP_FAKEERP_OAUTH_CLIENT_ID and EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE go together")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return fakeerp.Options{}, errors.New("fakeerp: cannot read the OAuth client secret file")
	}
	secret := strings.TrimRight(string(b), "\r\n")
	if secret == "" || len(secret) > 4096 {
		return fakeerp.Options{}, errors.New("fakeerp: invalid OAuth client secret file")
	}
	return fakeerp.Options{OAuthClientID: id, OAuthClientSecret: secret, TokenTTL: ttl}, nil
}

// loadFederated reads the optional federated client (ADR-019 Rev 1.1): its
// id, the issuer, audience and subject its assertions must name, and the
// issuer's JWKS file. All five come together.
func loadFederated(getenv func(string) string) (*fakeerp.Federated, error) {
	const p = "EACP_FAKEERP_OAUTH_FEDERATED_"
	names := []string{"CLIENT_ID", "ISSUER", "AUDIENCE", "SUBJECT", "JWKS_FILE"}
	v := map[string]string{}
	for _, n := range names {
		if s := getenv(p + n); s != "" {
			v[n] = s
		}
	}
	if len(v) == 0 {
		return nil, nil
	}
	if len(v) != len(names) {
		return nil, errors.New("fakeerp: the " + p + "* settings (CLIENT_ID, ISSUER, AUDIENCE, SUBJECT, JWKS_FILE) go together")
	}
	raw, err := os.ReadFile(v["JWKS_FILE"])
	if err != nil {
		return nil, errors.New("fakeerp: cannot read the federated JWKS file")
	}
	keys, err := fakeerp.ParseJWKS(raw)
	if err != nil {
		return nil, err
	}
	return &fakeerp.Federated{ClientID: v["CLIENT_ID"], Issuer: v["ISSUER"], Audience: v["AUDIENCE"],
		Subject: v["SUBJECT"], Keys: keys}, nil
}

func main() {
	service.Main("fakeerp", config.Options{RequireDatabase: false, DefaultHTTPAddr: ":8090"},
		func(d *service.Deps, mux *http.ServeMux) error {
			o, err := loadOAuth(os.Getenv)
			if err != nil {
				return err
			}
			if o.Federated, err = loadFederated(os.Getenv); err != nil {
				return err
			}
			h, token, err := loadHandler(os.Getenv("EACP_FAKEERP_TOKEN_FILE"), os.Getenv("EACP_FAKEERP_DATA_FILE"), o)
			if err != nil {
				return err
			}
			d.RedactSecrets(token, o.OAuthClientSecret)
			mux.Handle("/", h)
			return nil
		})
}
