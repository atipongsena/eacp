package governance_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/governance/conformance"
)

const referencePath = "../../test/conformance/governance_reference.json"

var update = flag.Bool("update", false, "rewrite the digests of the conformance reference set")

// TestConformanceReference: the local provider produces every expected
// outcome of the ADR-002 §7 reference set, digests included. The AGT
// sidecar's image build checks the same file through ACS.
func TestConformanceReference(t *testing.T) {
	f, err := conformance.Load(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[governance.Verdict]bool{}
	for i, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			req, err := f.Request(c)
			if err != nil {
				t.Fatal(err)
			}
			d, err := governance.EvaluateChecked(context.Background(), governance.LocalProvider{InstanceID: "conformance"}, req)
			if *update && err == nil && c.Expect.Error == "" {
				f.Cases[i].Expect.InputDigest = hex.EncodeToString(d.InputDigest[:])
				f.Cases[i].Expect.EnforcedDigest = hex.EncodeToString(d.EnforcedDigest[:])
				c = f.Cases[i]
			}
			if err := conformance.Check(c.Expect, d, err); err != nil {
				t.Fatal(err)
			}
			verdicts[c.Expect.Verdict] = true
			if c.AGTExpect != nil && c.AGTDivergence == "" {
				t.Fatal("an AGT override must document its divergence")
			}
		})
	}
	for _, v := range []governance.Verdict{governance.VerdictAllow, governance.VerdictWarn, governance.VerdictDeny,
		governance.VerdictEscalate, governance.VerdictTransform} {
		if !verdicts[v] {
			t.Errorf("reference set has no %s case", v)
		}
	}
	if *update {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(f); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(referencePath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
