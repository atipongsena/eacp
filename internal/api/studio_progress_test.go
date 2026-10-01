package api_test

import (
	"testing"
)

func TestStudioPreviewAndProgressRoutesAreFenced(t *testing.T) {
	h, _, version := studioRunHarness(t)
	preview := "/v1/studio/versions/" + version + "/previews"
	in := map[string]any{"inputs": map[string]string{"employee_id": "E-1"}, "samples": map[string]any{"lookup": map[string]any{"structuredContent": map[string]int{"days": 12}}}}
	code, body := h.as("sid", "POST", preview, in)
	h.want(403, code, body)
	code, run := h.as("stella", "POST", preview, in)
	h.want(201, code, run)
	if run["mode"] != "preview" {
		t.Fatalf("mode=%v", run["mode"])
	}
	code, claim := h.as("rt", "POST", "/v1/studio/runtime/claims", map[string]any{"runtime_id": "r1", "master_version": "v1", "lease_seconds": 30, "limit": 1})
	h.want(200, code, claim)
	base := "/v1/studio/runtime/runs/" + str(run, "id")
	lease := map[string]any{"runtime_id": "r1", "generation": 1, "index": 0}
	for _, suffix := range []string{"/nodes/begin", "/nodes/complete", "/nodes/output", "/llm/begin"} {
		code, body = h.as("stella", "POST", base+suffix, lease)
		h.want(403, code, body)
	}
	code, body = h.as("rt", "POST", base+"/nodes/begin", lease)
	h.want(200, code, body)
	code, body = h.as("rt", "POST", base+"/nodes/output", lease)
	h.want(200, code, body)
	if body["state"] != "succeeded" || body["output"] == nil {
		t.Fatalf("sample output missing")
	}
	lease["result"] = map[string]any{}
	code, body = h.as("rt", "POST", base+"/nodes/complete", lease)
	h.want(204, code, body)
	code, body = h.as("rt", "POST", base+"/finish", map[string]any{"runtime_id": "r1", "generation": 1, "state": "SUCCEEDED", "answer": "12 days"})
	h.want(204, code, body)
	code, body = h.as("rt", "POST", base+"/nodes/output", map[string]any{"runtime_id": "r1", "generation": 1, "index": 0})
	h.want(403, code, body)
}
