package worker

import "testing"

func TestDecideAppliesTheProofStandard(t *testing.T) {
	found := LookupResult{Status: LookupFound, ExternalReference: "PO-1"}
	absent := LookupResult{Status: LookupAbsent}
	unknown := LookupResult{Status: LookupUnknown}
	conflict := LookupResult{Status: LookupConflict}
	auth := reconcileFacts{Proof: "authoritative", Settled: true, RetryAllowed: true}
	best := reconcileFacts{Proof: "best_effort", Settled: true, RetryAllowed: true}
	with := func(f reconcileFacts, fn func(*reconcileFacts)) reconcileFacts { fn(&f); return f }

	for _, tc := range []struct {
		name      string
		facts     reconcileFacts
		lookup    LookupResult
		check, to string
	}{
		{"found succeeds", best, found, "found", "SUCCEEDED"},
		{"found even when exhausted", with(best, func(f *reconcileFacts) { f.Exhausted = true }), found, "found", "SUCCEEDED"},
		{"found agreeing with a reported success", with(auth, func(f *reconcileFacts) { f.Reported = []string{"PO-1"} }),
			found, "found", "SUCCEEDED"},
		{"found contradicting a reported success", with(auth, func(f *reconcileFacts) { f.Reported = []string{"PO-2"} }),
			found, "found", "NEEDS_HUMAN_RESOLUTION"},
		{"conflict", auth, conflict, "conflict", "NEEDS_HUMAN_RESOLUTION"},
		{"authoritative absence retries", auth, absent, "absent", "RETRY_WAIT"},
		{"authoritative absence without a retry fails",
			with(auth, func(f *reconcileFacts) { f.RetryAllowed = false }), absent, "absent", "FAILED"},
		{"authoritative absence contradicting a reported success",
			with(auth, func(f *reconcileFacts) { f.Reported = []string{"PO-1"} }), absent, "absent", "NEEDS_HUMAN_RESOLUTION"},
		{"authoritative absence before the call settled",
			with(auth, func(f *reconcileFacts) { f.Settled = false }), absent, "absent", "UNKNOWN_OUTCOME"},
		{"best-effort absence is still unknown", best, absent, "absent", "UNKNOWN_OUTCOME"},
		{"best-effort absence exhausted", with(best, func(f *reconcileFacts) { f.Exhausted = true }),
			absent, "absent", "NEEDS_HUMAN_RESOLUTION"},
		{"unknown lookup", auth, unknown, "unknown", "UNKNOWN_OUTCOME"},
		{"unknown lookup exhausted", with(auth, func(f *reconcileFacts) { f.Exhausted = true }),
			unknown, "unknown", "NEEDS_HUMAN_RESOLUTION"},
		{"unexpected proof standard never proves absence", with(auth, func(f *reconcileFacts) { f.Proof = "none" }),
			absent, "absent", "UNKNOWN_OUTCOME"},
		{"found without a reference is unknown", best, LookupResult{Status: LookupFound}, "unknown", "UNKNOWN_OUTCOME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decide(tc.facts, tc.lookup)
			if got.Check != tc.check || got.To != tc.to || got.Reason == "" {
				t.Fatalf("decide = %+v, want check %s to %s", got, tc.check, tc.to)
			}
			if got.To == "SUCCEEDED" && got.ExternalReference != "PO-1" {
				t.Fatalf("success reference = %q", got.ExternalReference)
			}
		})
	}
}
