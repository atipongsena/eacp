package a2a

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/worker"
)

// Task states (A2A 1.0).
const (
	stateSubmitted     = "TASK_STATE_SUBMITTED"
	stateWorking       = "TASK_STATE_WORKING"
	stateCompleted     = "TASK_STATE_COMPLETED"
	stateFailed        = "TASK_STATE_FAILED"
	stateCanceled      = "TASK_STATE_CANCELED"
	stateRejected      = "TASK_STATE_REJECTED"
	stateInputRequired = "TASK_STATE_INPUT_REQUIRED"
	stateAuthRequired  = "TASK_STATE_AUTH_REQUIRED"
)

const maxText = 65536

// preTaskCodes are the JSON-RPC errors the protocol layer returns before
// any task exists (parse, request, method, params, and A2A's unsupported
// operation, content type, extension and version). Every other code may
// follow real work, so it proves nothing.
var preTaskCodes = map[string]bool{"32700": true, "32600": true, "32601": true, "32602": true,
	"32004": true, "32005": true, "32008": true, "32009": true}

// knownState keeps remote content out of the log: a state EACP does not
// know is logged as "unknown".
func knownState(s string) string {
	switch s {
	case "":
		return ""
	case stateSubmitted, stateWorking, stateCompleted, stateFailed, stateCanceled, stateRejected,
		stateInputRequired, stateAuthRequired:
		return s
	}
	return "unknown"
}

type rpcError struct {
	Code json.Number `json:"code"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type task struct {
	ID     string `json:"id"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
	Artifacts json.RawMessage `json:"artifacts"`
}

// delegation is one Execute: one message, then the task it started.
type delegation struct {
	c      *Client
	call   worker.Call
	nextID int
	polls  int
	final  task
	worked bool // the agent reported the task WORKING
}

// Execute sends the enforced payload to the agent once, follows the task
// it starts and classifies the result (ADR-030 §4). A task that is left
// unfinished, waiting for input or interrupted gets one best-effort
// CancelTask; the outcome stays ambiguous.
func (c *Client) Execute(ctx context.Context, call worker.Call) worker.Result {
	if _, ok := parseEndpoint(call.Endpoint); !ok || call.Secret.Reveal() == "" {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_endpoint_or_credential"}
	}
	parts, ok := payloadParts(call.Payload)
	if !ok {
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "invalid_payload"}
	}
	d := &delegation{c: c, call: call}
	res := d.run(ctx, parts)
	c.Log.InfoContext(ctx, "a2a delegation", "host", hostOf(call.Endpoint), "action_id", call.ActionID,
		"task_id", d.final.ID, "state", knownState(d.final.Status.State), "polls", d.polls, "outcome", res.Outcome,
		"class", res.ErrorClass, "artifacts", artifactCount(d.final.Artifacts),
		"artifacts_sha256", digest(d.final.Artifacts))
	return res
}

// Lookup reports nothing: A2A defines no lookup by operation key, and an
// a2a contract never asks for one (ADR-030 §5).
func (c *Client) Lookup(context.Context, worker.LookupCall) worker.LookupResult {
	return worker.LookupResult{Status: worker.LookupUnknown}
}

// payloadParts validates the delegate payload (PayloadSchema) and returns
// its message parts.
func payloadParts(payload json.RawMessage) ([]any, bool) {
	var p map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(payload))
	if dec.Decode(&p) != nil || len(p) == 0 {
		return nil, false
	}
	var parts []any
	for k := range p {
		if k != "text" && k != "data" {
			return nil, false
		}
	}
	if raw, ok := p["text"]; ok {
		text, ok := str(raw)
		if !ok || text == "" || utf8.RuneCountInString(text) > maxText {
			return nil, false
		}
		parts = append(parts, map[string]any{"text": text})
	}
	if raw, ok := p["data"]; ok {
		if _, ok := object(raw); !ok {
			return nil, false
		}
		parts = append(parts, map[string]any{"data": raw, "mediaType": "application/json"})
	}
	return parts, true
}

func (d *delegation) run(ctx context.Context, parts []any) worker.Result {
	params := map[string]any{
		"message": map[string]any{"messageId": d.call.ActionID.String(), "role": "ROLE_USER", "parts": parts,
			"metadata": map[string]any{"eacp": map[string]any{"operation_key": d.call.OperationKey}}},
		"configuration": map[string]any{"returnImmediately": true},
	}
	resp, status, err := d.send(ctx, "SendMessage", params)
	switch {
	case errors.Is(err, errRefused):
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "connection_refused_before_send"}
	case err != nil && ctx.Err() != nil:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout"}
	case err != nil:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "transport_error"}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "unauthorized"}
	case status != http.StatusOK:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "http_" + strconv.Itoa(status)}
	case resp == nil:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	case resp.Error != nil:
		// A protocol-layer refusal starts no task; any other error may
		// follow real work.
		if code, ok := rpcCode(resp.Error); ok && preTaskCodes[code] {
			return worker.Result{Outcome: worker.NoEffect, ErrorClass: "a2a_rpc_" + code}
		} else if ok {
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_rpc_" + code}
		}
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(resp.Result, &result) != nil {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	}
	t, isTask := result["task"]
	m, isMessage := result["message"]
	switch {
	case isTask == isMessage || len(result) != 1:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	case isMessage:
		// A direct reply: the agent answered without a task.
		var msg struct {
			MessageID string          `json:"messageId"`
			Parts     json.RawMessage `json:"parts"`
		}
		if json.Unmarshal(m, &msg) != nil || !d.validID(msg.MessageID) {
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
		}
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: "message:" + msg.MessageID,
			Output: output("parts", msg.Parts)}
	}
	var first task
	if json.Unmarshal(t, &first) != nil || !d.validID(first.ID) {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	}
	return d.follow(ctx, first)
}

// follow polls the task while it runs and classifies where it ends.
func (d *delegation) follow(ctx context.Context, t task) worker.Result {
	id := t.ID
	wait := d.c.PollStart
	for {
		d.final = t
		switch t.Status.State {
		case stateCompleted:
			return worker.Result{Outcome: worker.Succeeded, ExternalReference: id, Output: output("artifacts", t.Artifacts)}
		case stateRejected:
			if d.worked {
				// Work was reported: a rejection now proves nothing.
				return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_rejected_after_work", RemoteReference: id}
			}
			return worker.Result{Outcome: worker.NoEffect, ErrorClass: "a2a_rejected", RemoteReference: id}
		case stateFailed:
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_failed", RemoteReference: id}
		case stateCanceled:
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_canceled", RemoteReference: id}
		case stateInputRequired, stateAuthRequired:
			// EACP never answers on the agent's behalf: contain the task.
			d.cancel(ctx, id)
			class := "a2a_input_required"
			if t.Status.State == stateAuthRequired {
				class = "a2a_auth_required"
			}
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: class, RemoteReference: id}
		case stateWorking:
			d.worked = true
		case stateSubmitted:
		default:
			d.cancel(ctx, id)
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response", RemoteReference: id}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			d.cancel(ctx, id)
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_interrupted", RemoteReference: id}
		case <-timer.C:
		}
		wait = min(2*wait, d.c.PollMax)

		d.polls++
		resp, status, err := d.send(ctx, "GetTask", map[string]any{"id": id})
		var next task
		switch {
		case err != nil && ctx.Err() != nil:
			d.cancel(ctx, id)
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_interrupted", RemoteReference: id}
		case err != nil:
			d.cancel(ctx, id)
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "transport_error", RemoteReference: id}
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "unauthorized", RemoteReference: id}
		case status != http.StatusOK || resp == nil || resp.Error != nil ||
			json.Unmarshal(resp.Result, &next) != nil || next.ID != id:
			d.cancel(ctx, id)
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response", RemoteReference: id}
		}
		t = next
	}
}

// cancel sends one best-effort CancelTask with its own budget: the call's
// context may already be over. It never changes the outcome.
func (d *delegation) cancel(ctx context.Context, id string) {
	cctx, done := context.WithTimeout(context.WithoutCancel(ctx), d.c.CancelBudget)
	defer done()
	resp, status, err := d.send(cctx, "CancelTask", map[string]any{"id": id})
	accepted := err == nil && status == http.StatusOK && resp != nil && resp.Error == nil
	d.c.Log.InfoContext(ctx, "a2a cancel requested", "host", hostOf(d.call.Endpoint), "action_id", d.call.ActionID,
		"task_id", id, "accepted", accepted)
}

var errRefused = errors.New("connection refused before send")

// send posts one JSON-RPC request. It returns the parsed response (nil
// when the body is not a JSON-RPC 2.0 response to this request) and the
// HTTP status.
func (d *delegation) send(ctx context.Context, method string, params map[string]any) (*rpcResponse, int, error) {
	d.nextID++
	id := strconv.Itoa(d.nextID)
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.call.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	// A POST is never replayed by the transport: a lost reply may follow an
	// applied message, so only the worker decides on another attempt.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("A2A-Version", Version)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	if err := d.call.Secret.Authorize(req, body); err != nil { // last: SigV4 signs every header above
		return nil, 0, err
	}
	resp, err := d.c.http.Do(req)
	if err != nil {
		if ctx.Err() == nil && refused(err) {
			return nil, 0, errRefused
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	b, err := readAll(resp.Body, maxResponseBytes)
	if errors.Is(err, errTooLarge) {
		return nil, resp.StatusCode, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var r rpcResponse
	if json.Unmarshal(b, &r) != nil || r.JSONRPC != "2.0" || (r.Error == nil && string(r.ID) != strconv.Quote(id)) ||
		(r.Error == nil) == (len(r.Result) == 0 || string(r.Result) == "null") {
		return nil, resp.StatusCode, nil
	}
	return &r, resp.StatusCode, nil
}

// validID accepts an id EACP may store and log: the attempt's pattern and
// never the credential.
func (d *delegation) validID(id string) bool {
	return remoteID.MatchString(id) && !d.call.Secret.Contains(id)
}

// rpcCode returns |code| of a JSON-RPC error, at most six digits.
func rpcCode(e *rpcError) (string, bool) {
	n, err := strconv.ParseInt(e.Code.String(), 10, 64)
	if err != nil {
		return "", false
	}
	if n < 0 {
		n = -n
	}
	if n > 999999 {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

func hostOf(endpoint string) string {
	if u, ok := parseEndpoint(endpoint); ok {
		return u.Host
	}
	return ""
}

func artifactCount(raw json.RawMessage) int {
	var a []json.RawMessage
	_ = json.Unmarshal(raw, &a)
	return len(a)
}

// digest is the SHA-256 of the artifacts' canonical JSON: evidence that
// they existed, never their content.
func digest(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	b, err := governance.Canonicalize(raw)
	if err != nil {
		b = raw
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// output wraps a success's artifacts or reply parts as its output (ADR-034):
// the worker keeps it only when the contract has a result retention. None
// (absent or null) is no output.
func output(name string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	b, err := json.Marshal(map[string]json.RawMessage{name: raw})
	if err != nil {
		return nil
	}
	return b
}
