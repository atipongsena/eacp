package studioruntime

import (
	"context"
	"encoding/json"
	"strconv"
)

func (x *execution) llmNode(ctx context.Context, index int, st step, env Env, intent node) (any, string, error) {
	if intent.CallID != nil {
		return x.nodeOutput(ctx, index)
	}
	if x.r.gateway == nil {
		return nil, "llm_failed", nil
	}
	instruction, err := renderText(st.Instruction, env)
	if err != nil {
		return nil, "result_unavailable", nil
	}
	input, err := decodeJSON(st.Input)
	if err != nil {
		return nil, "result_unavailable", nil
	}
	input, err = Render(input, env)
	if err != nil {
		return nil, "result_unavailable", nil
	}
	text, err := json.Marshal(input)
	if err != nil {
		return nil, "result_unavailable", nil
	}
	var models struct {
		Models []struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"models"`
	}
	if _, err = x.r.api.do(ctx, x.r.keys[x.tenant], "GET", "/v1/llm-models", nil, nil, &models); err != nil {
		return nil, "", err
	}
	provider := ""
	for _, m := range models.Models {
		if m.Name == st.Model {
			provider = m.Provider
			break
		}
	}
	path := "/v1/chat/completions"
	body := map[string]any{"model": st.Model, "max_tokens": st.MaxOutputTokens, "stream": false}
	user := map[string]string{"role": "user", "content": string(text)}
	switch provider {
	case "openai":
		body["messages"] = []any{map[string]string{"role": "system", "content": instruction}, user}
	case "anthropic":
		path = "/v1/messages"
		body["system"] = instruction
		body["messages"] = []any{user}
	default:
		return nil, "llm_failed", nil
	}
	headers := map[string]string{"EACP-Subject": x.c.Subject, "EACP-Studio-Intent": intent.IntentID.String(), "EACP-Studio-Runtime": x.r.id, "EACP-Studio-Generation": strconv.FormatInt(x.c.Generation, 10)}
	if provider == "anthropic" {
		headers["anthropic-version"] = "2023-06-01"
	}
	// Never retry this POST, even on an ambiguous transport or ledger error.
	status, sendErr := x.r.gateway.do(ctx, x.key, "POST", path, headers, body, nil)
	if status == 401 {
		reason, err := x.unauthorized(ctx)
		return nil, reason, err
	}
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	var durable node
	if err = x.nodeCall(ctx, index, "begin", nil, &durable); err != nil {
		return nil, "", err
	}
	if durable.CallID == nil {
		// No durable admission can be found. Fail this execution closed.
		// A takeover can re-fence only an intent with no admitted call.
		if sendErr != nil {
			return nil, "llm_unknown", nil
		}
		return nil, "llm_output_unavailable", nil
	}
	return x.nodeOutput(ctx, index)
}
