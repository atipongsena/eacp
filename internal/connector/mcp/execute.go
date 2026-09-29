package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/worker"
)

// protocolCodes are the JSON-RPC errors a server returns before it runs a
// tool (parse, request, method, params): certifiable as no effect. Every
// other code may follow a tool that ran (ADR-032 S3.4).
var protocolCodes = map[int]bool{-32700: true, -32600: true, -32601: true, -32602: true}

// Execute sends the enforced payload to the tool as one tools/call and
// classifies the reply (ADR-032 S3.3, S3.4). It never retries, never answers
// an input request and returns a digest of the result, never its content.
func (c *Client) Execute(ctx context.Context, call worker.Call) worker.Result {
	res := c.execute(ctx, call)
	c.Log.InfoContext(ctx, "mcp tool call", "host", hostOf(call.Endpoint), "action_id", call.ActionID,
		"outcome", res.Outcome, "class", res.ErrorClass, "reference", res.ExternalReference)
	return res
}

// Lookup reports nothing: MCP has no lookup by operation key, and an MCP
// contract never asks for one (ADR-032 S3.1).
func (c *Client) Lookup(context.Context, worker.LookupCall) worker.LookupResult {
	return worker.LookupResult{Status: worker.LookupUnknown}
}

func (c *Client) execute(ctx context.Context, call worker.Call) worker.Result {
	// A tools/call has no idempotency key: one attempt, or none at all.
	if call.Contract.IdempotencyMode != "none" || call.Contract.MaxAttempts != 1 || !toolName.MatchString(call.RemoteName) {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_contract"}
	}
	u, err := url.Parse(call.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" ||
		call.Secret.Reveal() == "" {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_endpoint_or_credential"}
	}
	if call.Secret.SignsRequests() { // a Bearer only; AWS keys are never sent as one
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "unsupported_credential"}
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(call.Payload, &args) != nil || args == nil {
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "invalid_payload"}
	}

	s := &session{c: c, endpoint: u.String(), secret: call.Secret.Reveal(), budget: c.maxBytes}
	_, modern, err := s.probe(ctx)
	if err != nil {
		return beforeCall(err)
	}
	if !modern {
		defer s.close()
		if _, err := s.initialize(ctx); err != nil {
			return beforeCall(err)
		}
	}

	result, err := s.call(ctx, "tools/call", map[string]any{"name": call.RemoteName, "arguments": call.Payload})
	if err != nil {
		return afterCall(ctx, err)
	}
	return classify(call, result)
}

// beforeCall classifies a failure before tools/call was sent: nothing was
// called, so every class is a no effect (ADR-032 S3.4).
func beforeCall(err error) worker.Result {
	switch {
	case errors.Is(err, errRefused):
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "connection_refused_before_send"}
	case worker.DiscoveryClass(err) == "unauthorized":
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "unauthorized"}
	default:
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "definition_unverified"}
	}
}

// afterCall classifies a tools/call that got no valid result. Only a
// refusal, an HTTP 401/403 and a protocol-layer JSON-RPC error prove that
// the tool did not run.
func afterCall(ctx context.Context, err error) worker.Result {
	var rpc *rpcError
	class := worker.DiscoveryClass(err)
	switch {
	case errors.Is(err, errRefused):
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "connection_refused_before_send"}
	case class == "unauthorized":
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "unauthorized"}
	case class == "too_large":
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "response_too_large"}
	case errors.Is(ctx.Err(), context.Canceled):
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "mcp_interrupted"}
	case ctx.Err() != nil:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout"}
	case errors.As(err, &rpc):
		// JSON-RPC error codes are negative; a positive one must not borrow a
		// certifiable class name.
		if rpc.Code >= 0 || rpc.Code < -999999 {
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
		}
		code := "mcp_rpc_" + strconv.Itoa(-rpc.Code)
		if protocolCodes[rpc.Code] {
			return worker.Result{Outcome: worker.NoEffect, ErrorClass: code}
		}
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: code}
	case class == "input_required":
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "mcp_input_required"}
	case class == "invalid_response":
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	case class == "http_status" || errors.Is(err, errNotModern):
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "http_status"}
	default:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "transport_error"}
	}
}

// classify maps a complete tools/call result: a tool error, output that
// breaks the certified outputSchema, or a success with the result's digest.
func classify(call worker.Call, result json.RawMessage) worker.Result {
	// A result is a JSON object; null (which a struct accepts) is not one.
	var r map[string]json.RawMessage
	if json.Unmarshal(result, &r) != nil || r == nil {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	}
	switch string(r["isError"]) {
	case "", "false":
	case "true":
		// A no effect only when the contract certifies it (ADR-032 S3.4):
		// the worker keeps a certified NoEffect but never upgrades an
		// Ambiguous.
		if slices.Contains(call.Contract.NoEffectErrors, "mcp_tool_error") {
			return worker.Result{Outcome: worker.NoEffect, ErrorClass: "mcp_tool_error"}
		}
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "mcp_tool_error"}
	default:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	}
	if sc, ok := r["structuredContent"]; ok && !validOutput(call.Definition, sc) {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "mcp_output_invalid"}
	}
	ref := reference(result)
	if call.Secret.Contains(ref) {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	}
	return worker.Result{Outcome: worker.Succeeded, ExternalReference: ref}
}

// validOutput reports whether structured is valid against the certified
// definition's outputSchema. No outputSchema accepts any output; a schema
// that does not resolve accepts none.
func validOutput(definition string, structured json.RawMessage) bool {
	var def struct {
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	if json.Unmarshal([]byte(definition), &def) != nil {
		return false
	}
	if len(def.OutputSchema) == 0 {
		return true
	}
	var schema jsonschema.Schema
	if json.Unmarshal(def.OutputSchema, &schema) != nil {
		return false
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return false
	}
	var v any
	if json.Unmarshal(structured, &v) != nil {
		return false
	}
	return resolved.Validate(v) == nil
}

// reference is mcp:sha256:<hex> of the result in RFC 8785 form (its raw
// bytes when it cannot be canonicalized): evidence that a result arrived,
// never its content.
func reference(result json.RawMessage) string {
	b, err := governance.Canonicalize(result)
	if err != nil {
		b = result
	}
	sum := sha256.Sum256(b)
	return "mcp:sha256:" + hex.EncodeToString(sum[:])
}

func hostOf(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Host
	}
	return ""
}
