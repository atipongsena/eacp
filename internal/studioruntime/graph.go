package studioruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

type node struct {
	Index     int        `json:"index"`
	Kind      string     `json:"kind"`
	State     string     `json:"state"`
	IntentID  uuid.UUID  `json:"intent_id"`
	ActionID  *uuid.UUID `json:"action_id"`
	CallID    *uuid.UUID `json:"call_id"`
	NextIndex *int       `json:"next_index"`
}

func (x *execution) nodeCall(ctx context.Context, index int, operation string, result any, out any) error {
	body := map[string]any{"runtime_id": x.r.id, "generation": x.c.Generation, "index": index}
	if result != nil {
		if operation == "action" {
			body["action_id"] = result
		} else {
			body["result"] = result
		}
	}
	for {
		status, err := x.r.api.do(ctx, x.r.keys[x.tenant], "POST", "/v1/studio/runtime/runs/"+x.c.ID.String()+"/nodes/"+operation, nil, body, out)
		if err == nil {
			return nil
		}
		if status == 403 {
			return errLost
		}
		if status >= 400 && status < 500 {
			return err
		}
		if err = x.pause(ctx, err); err != nil {
			return err
		}
	}
}

func (x *execution) nodeOutput(ctx context.Context, index int) (any, string, error) {
	for {
		var response struct {
			State   string          `json:"state"`
			Output  json.RawMessage `json:"output"`
			Failure string          `json:"failure"`
		}
		if err := x.nodeCall(ctx, index, "output", nil, &response); err != nil {
			return nil, "", err
		}
		switch response.State {
		case "succeeded":
			value, err := decodeJSON(response.Output)
			if err != nil {
				return nil, "llm_output_unavailable", nil
			}
			return value, "", nil
		case "failed":
			return nil, response.Failure, nil
		}
		if err := x.pause(ctx, nil); err != nil {
			return nil, "", err
		}
	}
}

func (x *execution) stepsV2(ctx context.Context) (string, string, error) {
	env := Env{Inputs: x.c.Inputs, Outputs: map[string]any{}}
	// Re-read only outputs on the durable visited path, through the original
	// action result channel or the live fenced Studio output channel.
	for _, node := range x.c.Nodes {
		if node.State != "completed" || node.Index >= x.c.CurrentIndex {
			continue
		}
		if node.Index < 0 || node.Index >= len(x.c.Definition.Steps) {
			return "", "", errors.New("invalid node metadata")
		}
		st := x.c.Definition.Steps[node.Index]
		var value any
		var reason string
		var err error
		if st.Kind == "tool_call" && x.c.Mode != "preview" {
			if node.ActionID == nil {
				return "", "result_unavailable", nil
			}
			value, reason, err = x.await(ctx, *node.ActionID, x.needed(node.Index))
		} else if st.Kind == "llm" || (st.Kind == "tool_call" && x.c.Mode == "preview") {
			value, reason, err = x.nodeOutput(ctx, node.Index)
		}
		if reason != "" || err != nil {
			return "", reason, err
		}
		if value != nil {
			env.Outputs[st.ID] = value
		}
	}
	steps := x.c.Definition.Steps
	index := x.c.CurrentIndex
	for visited := 0; visited < len(steps); visited++ {
		if index < 0 || index >= len(steps) {
			return "", "", errors.New("invalid graph cursor")
		}
		active, err := x.beat(ctx)
		if err != nil {
			return "", "", err
		}
		if !active {
			return "", "version_replaced", nil
		}
		st := steps[index]
		if st.Kind == "respond" {
			text, err := renderText(st.Text, env)
			if err != nil {
				return "", "result_unavailable", nil
			}
			if len(text) > maxAnswer {
				return "", "answer_too_large", nil
			}
			return text, "", nil
		}
		var current node
		if err := x.nodeCall(ctx, index, "begin", nil, &current); err != nil {
			return "", "", err
		}
		destination := st.Next
		switch st.Kind {
		case "branch":
			left, err := decodeJSON(st.Condition.Left)
			if err != nil {
				return "", "branch_invalid", nil
			}
			left, err = Render(left, env)
			if err != nil {
				return "", "branch_invalid", nil
			}
			right, err := decodeJSON(st.Condition.Right)
			if err != nil {
				return "", "branch_invalid", nil
			}
			right, err = Render(right, env)
			if err != nil {
				return "", "branch_invalid", nil
			}
			choice, err := evaluateBranch(left, st.Condition.Operator, right)
			if err != nil {
				return "", "branch_invalid", nil
			}
			if err = x.nodeCall(ctx, index, "complete", map[string]bool{"choice": choice}, nil); err != nil {
				return "", "", err
			}
			destination = st.Else
			if choice {
				destination = st.Then
			}
		case "tool_call":
			var value any
			var reason string
			var err error
			completion := map[string]any{}
			if x.c.Mode == "preview" {
				value, reason, err = x.nodeOutput(ctx, index)
			} else {
				id := uuid.Nil
				if current.ActionID != nil {
					id = *current.ActionID
				}
				if id == uuid.Nil {
					id, reason, err = x.submit(ctx, index, st, env)
					if reason != "" || err != nil {
						return "", reason, err
					}
					if err = x.nodeCall(ctx, index, "action", id, nil); err != nil {
						return "", "", err
					}
				}
				value, reason, err = x.await(ctx, id, x.needed(index))
				completion["action_id"] = id
			}
			if reason != "" || err != nil {
				return "", reason, err
			}
			if value != nil {
				env.Outputs[st.ID] = value
			}
			if err = x.nodeCall(ctx, index, "complete", completion, nil); err != nil {
				return "", "", err
			}
		case "llm":
			value, reason, err := x.llmNode(ctx, index, st, env, current)
			if reason != "" || err != nil {
				return "", reason, err
			}
			env.Outputs[st.ID] = value
			if err = x.nodeCall(ctx, index, "complete", map[string]any{}, nil); err != nil {
				return "", "", err
			}
		default:
			return "", "", errors.New("unsupported graph node")
		}
		if x.c.Definition.SchemaVersion == 1 {
			index++
			continue
		}
		next := -1
		for i, step := range steps {
			if step.ID == destination {
				next = i
				break
			}
		}
		if next <= index {
			return "", "", errors.New("invalid graph successor")
		}
		index = next
	}
	return "", "", errors.New("graph has no response")
}
