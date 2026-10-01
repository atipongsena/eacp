package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry"
)

// WithA2A opts into ADR-030's inbound transport on the existing API listener.
// Startup validates publicURL through config.Load; blank keeps both routes off.
func (s *Server) WithA2A(publicURL string) *Server {
	s.a2aURL = publicURL
	return s
}

func (s *Server) a2aCard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("A2A-Version", "1.0")
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "EACP Governed Actions", "version": "1.0.0",
		"description":         "Delegate one structured enterprise action under existing EACP governance. An approved EACP agent key is required. SendMessage requires returnImmediately=true; no conversation history.",
		"supportedInterfaces": []any{map[string]any{"url": s.a2aURL, "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}},
		"capabilities":        map[string]any{"streaming": false, "pushNotifications": false},
		"defaultInputModes":   []string{"application/json"}, "defaultOutputModes": []string{"application/json"},
		"securitySchemes":      map[string]any{"eacp_agent": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer", "bearerFormat": "EACP agent key"}}},
		"securityRequirements": []any{map[string]any{"eacp_agent": []string{}}},
		"skills": []any{map[string]any{"id": "governed_action", "name": "Governed action",
			"description": "One JSON data part with subject, operation, target, tool, tool_schema_version, resource and payload. Fixed one-hour lifetime. Tools must already be approved and allowed for the caller.",
			"tags":        []string{"governance", "actions"}}},
	})
}

var a2aIdentifier = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)

type a2aEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type a2aFault struct {
	code    int
	message string
}

func (e *a2aFault) Error() string { return e.message }

func a2aInvalid() *a2aFault     { return &a2aFault{-32602, "Invalid params"} }
func a2aUnsupported() *a2aFault { return &a2aFault{-32004, "Unsupported operation"} }

// a2aDecode is deliberately closed, after the whole body has passed I-JSON
// validation. Nested RawMessages are checked separately before any core call.
func a2aDecode(raw []byte, dst any) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(dst) == nil
}

func a2aReply(w http.ResponseWriter, id json.RawMessage, result any, fault *a2aFault) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if fault != nil {
		out["error"] = map[string]any{"code": fault.code, "message": fault.message}
	} else {
		out["result"] = result
	}
	w.Header().Set("A2A-Version", "1.0")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) a2aRPC(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	// Never pass protocol/content errors to finish: it can log raw errors.
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		a2aReply(w, nil, nil, &a2aFault{-32600, "Invalid request"})
		return nil
	}
	canonical, err := governance.Canonicalize(raw)
	if err != nil {
		a2aReply(w, nil, nil, &a2aFault{-32700, "Parse error"})
		return nil
	}
	var req a2aEnvelope
	if !a2aDecode(canonical, &req) || req.JSONRPC != "2.0" || !a2aRequestID(req.ID) {
		a2aReply(w, nil, nil, &a2aFault{-32600, "Invalid request"})
		return nil
	}
	if r.Header.Get("A2A-Version") != "1.0" {
		a2aReply(w, req.ID, nil, &a2aFault{-32009, "Version not supported"})
		return nil
	}
	var result any
	var fault *a2aFault
	switch req.Method {
	case "SendMessage":
		result, fault = s.a2aSend(r, c, req.Params)
	case "GetTask", "CancelTask":
		result, fault = s.a2aFollow(r, c, req.Method, req.Params)
	case "SendStreamingMessage", "SubscribeToTask", "ListTasks", "GetExtendedAgentCard",
		"CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig", "ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig":
		fault = a2aUnsupported()
	default:
		fault = &a2aFault{-32601, "Method not found"}
	}
	a2aReply(w, req.ID, result, fault)
	return nil
}

func a2aRequestID(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var id any
	if json.Unmarshal(raw, &id) != nil {
		return false
	}
	switch v := id.(type) {
	case string:
		return a2aIdentifier.MatchString(v)
	case float64:
		return true // the I-JSON validator already enforces numeric bounds
	}
	return false
}

type a2aSendParams struct {
	Tenant  string `json:"tenant"`
	Message *struct {
		ID         string          `json:"messageId"`
		Role       string          `json:"role"`
		TaskID     string          `json:"taskId"`
		ContextID  string          `json:"contextId"`
		References []string        `json:"referenceTaskIds"`
		Extensions []string        `json:"extensions"`
		Metadata   json.RawMessage `json:"metadata"`
		Parts      []struct {
			Data      json.RawMessage `json:"data"`
			MediaType string          `json:"mediaType"`
			Metadata  json.RawMessage `json:"metadata"`
		} `json:"parts"`
	} `json:"message"`
	Config *struct {
		ReturnImmediately bool            `json:"returnImmediately"`
		HistoryLength     int             `json:"historyLength"`
		Modes             []string        `json:"acceptedOutputModes"`
		Push              json.RawMessage `json:"taskPushNotificationConfig"`
	} `json:"configuration"`
	Metadata json.RawMessage `json:"metadata"`
}

type a2aActionData struct {
	Subject   string          `json:"subject"`
	Operation string          `json:"operation"`
	Target    string          `json:"target"`
	Tool      string          `json:"tool"`
	Schema    string          `json:"tool_schema_version"`
	Resource  string          `json:"resource"`
	Payload   json.RawMessage `json:"payload"`
}

func (s *Server) a2aSend(r *http.Request, c identity.Caller, raw []byte) (any, *a2aFault) {
	var p a2aSendParams
	if !a2aDecode(raw, &p) || p.Message == nil || !a2aIdentifier.MatchString(p.Message.ID) || p.Message.Role != "ROLE_USER" {
		return nil, a2aInvalid()
	}
	if p.Tenant != "" || p.Message.TaskID != "" || p.Message.ContextID != "" || len(p.Message.References) != 0 || len(p.Message.Extensions) != 0 || p.Config == nil || !p.Config.ReturnImmediately || p.Config.HistoryLength != 0 || len(p.Config.Push) != 0 {
		return nil, a2aUnsupported()
	}
	for _, mode := range p.Config.Modes {
		if mode != "application/json" {
			return nil, &a2aFault{-32005, "Content type not supported"}
		}
	}
	if len(p.Message.Parts) != 1 || (p.Message.Parts[0].MediaType != "" && p.Message.Parts[0].MediaType != "application/json") {
		return nil, &a2aFault{-32005, "Content type not supported"}
	}
	var data a2aActionData
	if !a2aDecode(p.Message.Parts[0].Data, &data) {
		return nil, a2aInvalid()
	}
	digest := sha256.Sum256([]byte(p.Message.ID))
	v, err := s.actions.Submit(r.Context(), actionActor(c), action.Submission{
		IdempotencyKey: "a2a:" + hex.EncodeToString(digest[:]), Subject: data.Subject, Operation: data.Operation,
		Target: data.Target, Tool: data.Tool, ToolSchemaVersion: data.Schema, Resource: data.Resource,
		Payload: data.Payload, Lifetime: time.Hour,
	})
	if v.ID == uuid.Nil {
		return nil, a2aCoreFault(err)
	}
	// A core error after commit must not erase the task or prove no effect.
	task, fault := s.a2aTask(r, c, v)
	if fault != nil {
		return nil, fault
	}
	return map[string]any{"task": task}, nil
}

type a2aTaskParams struct {
	ID            string          `json:"id"`
	Tenant        string          `json:"tenant"`
	HistoryLength int             `json:"historyLength"`
	Metadata      json.RawMessage `json:"metadata"`
}

func (s *Server) a2aFollow(r *http.Request, c identity.Caller, method string, raw []byte) (any, *a2aFault) {
	var p a2aTaskParams
	if !a2aDecode(raw, &p) {
		return nil, a2aInvalid()
	}
	if p.Tenant != "" || p.HistoryLength != 0 {
		return nil, a2aUnsupported()
	}
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, a2aInvalid()
	}
	v, err := s.actions.Read(r.Context(), actionActor(c), id)
	if err != nil {
		return nil, a2aCoreFault(err)
	}
	if method == "CancelTask" && v.State != "CANCELLED" {
		v, err = s.actions.Cancel(r.Context(), actionActor(c), id, "A2A caller requested cancellation")
		if err != nil {
			if errors.Is(err, registry.ErrConflict) {
				return nil, &a2aFault{-32002, "Task not cancelable"}
			}
			return nil, a2aCoreFault(err)
		}
		if v.State != "CANCELLED" {
			return nil, &a2aFault{-32002, "Task not cancelable"}
		}
	}
	return s.a2aTask(r, c, v)
}

func a2aCoreFault(err error) *a2aFault {
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, registry.ErrNotFound):
		return &a2aFault{-32001, "Task not found"}
	case errors.Is(err, registry.ErrInvalid):
		return a2aInvalid()
	case errors.Is(err, registry.ErrForbidden):
		return &a2aFault{-31403, "Unauthorized"}
	case errors.Is(err, action.ErrIdempotencyConflict):
		return &a2aFault{-32000, "Message ID conflicts with an earlier delegation"}
	case errors.Is(err, action.ErrAdmission):
		return &a2aFault{-32000, "Admission limit reached"}
	default:
		return &a2aFault{-32603, "Internal error"}
	}
}

func a2aState(state string) (string, string) {
	switch state {
	case "RECEIVED":
		return "TASK_STATE_SUBMITTED", state
	case "SUCCEEDED":
		return "TASK_STATE_COMPLETED", state
	case "DENIED":
		return "TASK_STATE_REJECTED", state
	case "FAILED", "EXPIRED":
		return "TASK_STATE_FAILED", state
	case "CANCELLED":
		return "TASK_STATE_CANCELED", state
	case "PENDING_APPROVAL", "AUTHORIZED", "QUEUED", "LEASED", "EXECUTING", "RETRY_WAIT", "UNKNOWN_OUTCOME", "NEEDS_HUMAN_RESOLUTION":
		return "TASK_STATE_WORKING", state
	default:
		return "TASK_STATE_WORKING", "unknown"
	}
}

func (s *Server) a2aTask(r *http.Request, c identity.Caller, v action.View) (any, *a2aFault) {
	state, safe := a2aState(v.State)
	task := map[string]any{"id": v.ID.String(), "contextId": v.ID.String(),
		"status":   map[string]any{"state": state, "timestamp": v.StateChangedAt.UTC().Format(time.RFC3339Nano)},
		"metadata": map[string]any{"eacp": map[string]any{"action_id": v.ID.String(), "state": safe}}}
	if v.State == "SUCCEEDED" {
		result, err := s.actions.Result(r.Context(), actionActor(c), v.ID)
		if err == nil {
			task["artifacts"] = []any{map[string]any{"artifactId": v.ID.String(), "parts": []any{map[string]any{"data": result.Output, "mediaType": "application/json"}}}}
		} else {
			var withheld *action.WithheldError
			if !errors.Is(err, action.ErrResultNotAvailable) && !errors.As(err, &withheld) {
				return nil, a2aCoreFault(err)
			}
		}
	}
	return task, nil
}
