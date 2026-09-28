// Command fakeerp is the credential-protected Slice A demo target. It keeps
// operation and audit evidence in a durable append log for lookup.
package main

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/atipongsena/eacp/internal/config"
	"github.com/atipongsena/eacp/internal/fakeerp"
	"github.com/atipongsena/eacp/internal/service"
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

// loadKeyClient reads the optional private_key_jwt client (ADR-019 Rev 1.2):
// its id, the audience its assertions must name (the token endpoint URL as
// the worker sees it) and the JWKS of its public keys. All three come
// together.
func loadKeyClient(getenv func(string) string) (*fakeerp.KeyClient, error) {
	id, aud, file := getenv("EACP_FAKEERP_OAUTH_KEY_CLIENT_ID"), getenv("EACP_FAKEERP_OAUTH_KEY_AUDIENCE"),
		getenv("EACP_FAKEERP_OAUTH_KEY_JWKS_FILE")
	if id == "" && aud == "" && file == "" {
		return nil, nil
	}
	if id == "" || aud == "" || file == "" {
		return nil, errors.New("fakeerp: EACP_FAKEERP_OAUTH_KEY_CLIENT_ID, _KEY_AUDIENCE and _KEY_JWKS_FILE go together")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, errors.New("fakeerp: cannot read the key client's JWKS file")
	}
	keys, err := fakeerp.ParseKeySet(raw)
	if err != nil {
		return nil, err
	}
	return &fakeerp.KeyClient{ClientID: id, Audience: aud, Keys: keys}, nil
}

// settings reads the variables prefix+names: all of them, or none (nil).
func settings(getenv func(string) string, prefix string, names ...string) (map[string]string, error) {
	v := map[string]string{}
	for _, n := range names {
		if s := getenv(prefix + n); s != "" {
			v[n] = s
		}
	}
	if len(v) == 0 {
		return nil, nil
	}
	if len(v) != len(names) {
		return nil, errors.New("fakeerp: the " + prefix + "* settings go together")
	}
	return v, nil
}

// spiffeKeys reads the JWT authorities of a SPIFFE bundle file.
func spiffeKeys(path string) ([]fakeerp.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("fakeerp: cannot read the SPIFFE bundle file")
	}
	return fakeerp.ParseSPIFFEBundle(raw)
}

// loadSPIFFEClient reads the optional OAuth client that authenticates with
// JWT-SVID assertions (ADR-019 Rev 1.4): its id, the issuer, audience and
// subject its SVIDs must name, and SPIRE's bundle. All five come together.
func loadSPIFFEClient(getenv func(string) string) (*fakeerp.SPIFFEClient, error) {
	v, err := settings(getenv, "EACP_FAKEERP_OAUTH_SPIFFE_", "CLIENT_ID", "ISSUER", "AUDIENCE", "SUBJECT", "BUNDLE_FILE")
	if v == nil || err != nil {
		return nil, err
	}
	keys, err := spiffeKeys(v["BUNDLE_FILE"])
	if err != nil {
		return nil, err
	}
	return &fakeerp.SPIFFEClient{ClientID: v["CLIENT_ID"], Issuer: v["ISSUER"], Audience: v["AUDIENCE"],
		Subject: v["SUBJECT"], Keys: keys}, nil
}

// loadSPIFFEBearer reads the optional JWT-SVID bearer of the ERP API: the
// audience and subject the SVIDs must name, and SPIRE's bundle. All three
// come together.
func loadSPIFFEBearer(getenv func(string) string) (*fakeerp.SPIFFEBearer, error) {
	v, err := settings(getenv, "EACP_FAKEERP_SPIFFE_", "AUDIENCE", "SUBJECT", "BUNDLE_FILE")
	if v == nil || err != nil {
		return nil, err
	}
	keys, err := spiffeKeys(v["BUNDLE_FILE"])
	if err != nil {
		return nil, err
	}
	return &fakeerp.SPIFFEBearer{Audience: v["AUDIENCE"], Subject: v["SUBJECT"], Keys: keys}, nil
}

// loadExchange reads the optional STS (ADR-019 Rev 1.5): the audience a
// token exchange must name and its trusted subjects, a Kubernetes
// service-account token (EACP_FAKEERP_STS_K8S_*: issuer, audience, subject,
// JWKS file) and a JWT-SVID (EACP_FAKEERP_STS_SPIFFE_*: issuer, audience,
// subject, SPIRE's bundle), each group all or none; and the service
// accounts exchanged tokens may impersonate (a comma-separated list).
func loadExchange(getenv func(string) string) (*fakeerp.TokenExchange, *fakeerp.Impersonation, error) {
	var imp *fakeerp.Impersonation
	if v := getenv("EACP_FAKEERP_IMPERSONATE_ACCOUNTS"); v != "" {
		imp = &fakeerp.Impersonation{Accounts: strings.Split(v, ",")}
	}
	subjects, err := loadSubjects(getenv, "EACP_FAKEERP_STS_")
	if err != nil {
		return nil, nil, err
	}
	audience := getenv("EACP_FAKEERP_STS_AUDIENCE")
	switch {
	case audience == "" && len(subjects) == 0 && imp == nil:
		return nil, nil, nil
	case audience == "" || len(subjects) == 0:
		return nil, nil, errors.New("fakeerp: EACP_FAKEERP_STS_AUDIENCE needs a subject group, and a subject group or impersonation needs it")
	}
	return &fakeerp.TokenExchange{Audience: audience, Subjects: subjects}, imp, nil
}

// loadSubjects reads the trusted subject tokens under prefix: a Kubernetes
// service-account token (prefix+K8S_*: issuer, audience, subject, JWKS file)
// and a JWT-SVID (prefix+SPIFFE_*: issuer, audience, subject, SPIRE's
// bundle), each group all or none.
func loadSubjects(getenv func(string) string, prefix string) ([]fakeerp.ExchangeSubject, error) {
	var subjects []fakeerp.ExchangeSubject
	k8s, err := settings(getenv, prefix+"K8S_", "ISSUER", "AUDIENCE", "SUBJECT", "JWKS_FILE")
	if err != nil {
		return nil, err
	}
	if k8s != nil {
		raw, err := os.ReadFile(k8s["JWKS_FILE"])
		if err != nil {
			return nil, errors.New("fakeerp: cannot read the " + prefix + "K8S_JWKS_FILE")
		}
		keys, err := fakeerp.ParseKeySet(raw)
		if err != nil {
			return nil, err
		}
		subjects = append(subjects, fakeerp.ExchangeSubject{Issuer: k8s["ISSUER"], Audience: k8s["AUDIENCE"],
			Subject: k8s["SUBJECT"], Keys: keys})
	}
	svid, err := settings(getenv, prefix+"SPIFFE_", "ISSUER", "AUDIENCE", "SUBJECT", "BUNDLE_FILE")
	if err != nil {
		return nil, err
	}
	if svid != nil {
		keys, err := spiffeKeys(svid["BUNDLE_FILE"])
		if err != nil {
			return nil, err
		}
		subjects = append(subjects, fakeerp.ExchangeSubject{Issuer: svid["ISSUER"], Audience: svid["AUDIENCE"],
			Subject: svid["SUBJECT"], Keys: keys})
	}
	return subjects, nil
}

// loadAWS reads the optional AWS STS (ADR-019 Rev 1.6): the role, region and
// service (EACP_FAKEERP_AWS_ROLE_ARN, _REGION, _SERVICE, all or none) and
// the web identities it trusts (EACP_FAKEERP_AWS_K8S_* and
// EACP_FAKEERP_AWS_SPIFFE_*, as for the token exchange). A role needs a
// subject, and a subject needs the role.
func loadAWS(getenv func(string) string) (*fakeerp.AWS, error) {
	role, err := settings(getenv, "EACP_FAKEERP_AWS_", "ROLE_ARN", "REGION", "SERVICE")
	if err != nil {
		return nil, err
	}
	subjects, err := loadSubjects(getenv, "EACP_FAKEERP_AWS_")
	if err != nil {
		return nil, err
	}
	switch {
	case role == nil && len(subjects) == 0:
		return nil, nil
	case role == nil || len(subjects) == 0:
		return nil, errors.New("fakeerp: the EACP_FAKEERP_AWS_ role and a subject group go together")
	}
	return &fakeerp.AWS{RoleARN: role["ROLE_ARN"], Region: role["REGION"], Service: role["SERVICE"], Subjects: subjects}, nil
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
			if o.KeyClient, err = loadKeyClient(os.Getenv); err != nil {
				return err
			}
			if o.SPIFFEClient, err = loadSPIFFEClient(os.Getenv); err != nil {
				return err
			}
			if o.SPIFFEBearer, err = loadSPIFFEBearer(os.Getenv); err != nil {
				return err
			}
			if o.Exchange, o.Impersonation, err = loadExchange(os.Getenv); err != nil {
				return err
			}
			if o.AWS, err = loadAWS(os.Getenv); err != nil {
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
