package identity

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNewKeyRoundTrips(t *testing.T) {
	tenant, cred := uuid.New(), uuid.New()
	key, hash, err := NewKey(KindAgent, tenant, cred)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "eacp_ak_") {
		t.Fatalf("key %q lacks agent prefix", key)
	}
	p, err := ParseKey(key)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if p.Kind != KindAgent || p.TenantID != tenant || p.CredentialID != cred {
		t.Fatalf("parsed %+v, want kind=ak tenant=%s cred=%s", p, tenant, cred)
	}
	if !p.Matches(hash) {
		t.Fatal("parsed key does not match its own hash")
	}
	if len(hash) != 32 {
		t.Fatalf("hash length %d, want 32 (sha-256)", len(hash))
	}
}

func TestNewKeyIsRandom(t *testing.T) {
	tenant, cred := uuid.New(), uuid.New()
	k1, h1, _ := NewKey(KindPrincipal, tenant, cred)
	k2, h2, _ := NewKey(KindPrincipal, tenant, cred)
	if k1 == k2 || bytes.Equal(h1, h2) {
		t.Fatal("two keys for the same ids are identical")
	}
}

func TestHashDoesNotContainSecret(t *testing.T) {
	key, hash, _ := NewKey(KindAgent, uuid.New(), uuid.New())
	p, _ := ParseKey(key)
	if bytes.Contains(hash, p.secret) {
		t.Fatal("stored hash contains the raw secret")
	}
}

func TestMatchesRejectsWrongSecret(t *testing.T) {
	tenant, cred := uuid.New(), uuid.New()
	_, hash, _ := NewKey(KindAgent, tenant, cred)
	other, _, _ := NewKey(KindAgent, tenant, cred)
	p, _ := ParseKey(other)
	if p.Matches(hash) {
		t.Fatal("a different secret matched")
	}
	if p.Matches(nil) || p.Matches(hash[:16]) {
		t.Fatal("malformed stored hash matched")
	}
}

func TestParseKeyRejectsMalformed(t *testing.T) {
	good, _, _ := NewKey(KindAgent, uuid.New(), uuid.New())
	parts := strings.SplitN(good, "_", 5)
	cases := map[string]string{
		"empty":           "",
		"no prefix":       strings.TrimPrefix(good, "eacp_"),
		"wrong prefix":    "xacp_" + strings.Join(parts[1:], "_"),
		"unknown kind":    "eacp_zz_" + strings.Join(parts[2:], "_"),
		"bad tenant":      "eacp_ak_nothex_" + strings.Join(parts[3:], "_"),
		"nil tenant":      "eacp_ak_" + strings.Repeat("0", 32) + "_" + strings.Join(parts[3:], "_"),
		"nil credential":  "eacp_ak_" + parts[2] + "_" + strings.Repeat("0", 32) + "_" + parts[4],
		"dashed uuid":     "eacp_ak_" + uuid.New().String() + "_" + strings.Join(parts[3:], "_"),
		"short secret":    good[:len(good)-4],
		"long secret":     good + "AAAA",
		"bad base64":      good[:len(good)-1] + "!",
		"missing secret":  strings.Join(parts[:4], "_"),
		"whitespace":      " " + good,
		"trailing spaces": good + " ",
	}
	for name, k := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKey(k); err == nil {
				t.Fatalf("ParseKey(%q) accepted a malformed key", k)
			}
		})
	}
}

func TestParsedKeyNeverPrintsSecret(t *testing.T) {
	key, _, _ := NewKey(KindAgent, uuid.New(), uuid.New())
	p, _ := ParseKey(key)
	secret := strings.SplitN(key, "_", 5)[4]
	for _, s := range []string{p.String(), strings.TrimSpace(fmt.Sprintf("%v %+v %#v", p, p, p))} {
		if strings.Contains(s, secret) {
			t.Fatalf("formatted ParsedKey leaks the secret: %s", s)
		}
	}
}
