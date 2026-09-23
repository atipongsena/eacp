package governance

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestCanonicalizeRFC8785(t *testing.T) {
	in := []byte(`{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001],"string":"\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/","literals":[null,true,false]}`)
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`
	got, err := canonicalize(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalizeSortsUTF16CodeUnits(t *testing.T) {
	got, err := canonicalize([]byte(`{"\ufb33":1,"\ud83d\ude00":2,"\u20ac":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"€":3,"😀":2,"דּ":1}`; string(got) != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalizeRejectsAmbiguousJSON(t *testing.T) {
	for _, in := range [][]byte{
		[]byte(`{"a":1,"a":2}`),
		[]byte(`{"a":"\ud800"}`),
		[]byte{'{', '"', 'a', '"', ':', '"', 0xff, '"', '}'},
		[]byte(`{"n":9007199254740993}`),
	} {
		if got, err := canonicalize(in); err == nil {
			t.Errorf("canonicalize(%q) = %s, want error", in, got)
		}
	}
}

func TestCanonicalizeNumberBoundaries(t *testing.T) {
	in := []byte(`[-0,1e-7,1e-6,1e20,1e21,9007199254740992]`)
	want := `[0,1e-7,0.000001,100000000000000000000,1e+21,9007199254740992]`
	got, err := canonicalize(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestDigestsBindTheEnforcedPayload(t *testing.T) {
	b := Binding{
		TenantID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		AgentID:        uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		AgentVersionID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Subject:        "buyer@example.test", Operation: "purchase", Target: "erp",
		Tool: "erp.create_order", ToolSchemaVersion: "1", Resource: "orders",
		Payload: json.RawMessage(`{"amount":2400000,"currency":"THB"}`),
	}
	input, enforced, err := Digests(b, json.RawMessage(`{"currency":"THB","amount":2400000}`))
	if err != nil {
		t.Fatal(err)
	}
	if input != enforced {
		t.Fatal("equivalent payloads have different digests")
	}
	changed, transformed, err := Digests(b, json.RawMessage(`{"amount":1000000,"currency":"THB"}`))
	if err != nil {
		t.Fatal(err)
	}
	if changed != input || transformed == input {
		t.Fatal("transform changed the input digest or did not change the enforced digest")
	}
	b.Payload = json.RawMessage(`{"amount":24000000,"currency":"THB"}`)
	substituted, _, err := Digests(b, json.RawMessage(`{"amount":1000000,"currency":"THB"}`))
	if err != nil {
		t.Fatal(err)
	}
	if substituted == input {
		t.Fatal("parameter substitution retained the input digest")
	}

	canonical := `{"agent":"22222222-2222-2222-2222-222222222222","agent_version":"33333333-3333-3333-3333-333333333333","operation":"purchase","payload":{"amount":2400000,"currency":"THB"},"resource":"orders","subject":"buyer@example.test","target":"erp","tenant":"11111111-1111-1111-1111-111111111111","tool":"erp.create_order","tool_schema_version":"1"}`
	if want := sha256.Sum256([]byte(canonical)); input != want {
		t.Fatalf("input digest = %x, want %x", input, want)
	}
}
