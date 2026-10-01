package fakellm_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeLLMStudioAnswersAreFixedAndContentFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.jsonl")
	s := serve(t, path)
	for _, api := range []string{"openai", "anthropic"} {
		for scenario, want := range map[string]string{"studio_true": `{"eligible":true}`, "studio_false": `{"eligible":false}`, "studio_invalid": "not-json"} {
			prompt := `{"request":"` + scenario + `","note":"STUDIO-PROMPT-CANARY"}`
			body, endpoint, headers := chat(prompt, false), "/v1/chat/completions", oai
			if api == "anthropic" {
				body, endpoint, headers = messages(prompt, false), "/v1/messages", anth
			}
			var req map[string]any
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatal(err)
			}
			req["model"] = "studio-triage"
			b, _ := json.Marshal(req)
			response, b := call(t, s, endpoint, string(b), headers)
			if response.StatusCode != 200 {
				t.Fatal("provider refused")
			}
			var output map[string]any
			if err := json.Unmarshal(b, &output); err != nil {
				t.Fatal(err)
			}
			text := ""
			if api == "anthropic" {
				text = output["content"].([]any)[0].(map[string]any)["text"].(string)
			} else {
				text = output["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
			}
			if text != want {
				t.Fatalf("%s/%s did not return its fixed typed fixture", api, scenario)
			}
		}
	}
	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"STUDIO-PROMPT-CANARY", key, `eligible`, "not-json"} {
		if strings.Contains(string(log), canary) {
			t.Fatal("provider audit retained content")
		}
	}
}
