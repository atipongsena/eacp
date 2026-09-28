// Package fakea2a is a credential-protected A2A 1.0 agent for tests and the
// A2A demo (ADR-030). It speaks JSON-RPC at /a2a, serves its Agent Card
// (re-read from a file on every request, so a demo can make it drift; the
// built-in card while that file does not exist) and
// keeps a durable log of every message and task: ids, states and the
// SHA-256 of each message, never its content. A message's data part picks
// the behaviour with "scenario":
//
//	(none)          COMPLETED at once, with one artifact
//	working         WORKING until delay_ms (default 1000), then COMPLETED
//	input_required  INPUT_REQUIRED
//	fail            FAILED
//	reject          REJECTED
//	hang            WORKING until CancelTask
package fakea2a

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/atipongsena/eacp/internal/governance"
)

// Task states (A2A 1.0).
const (
	stateWorking       = "TASK_STATE_WORKING"
	stateCompleted     = "TASK_STATE_COMPLETED"
	stateFailed        = "TASK_STATE_FAILED"
	stateCanceled      = "TASK_STATE_CANCELED"
	stateRejected      = "TASK_STATE_REJECTED"
	stateInputRequired = "TASK_STATE_INPUT_REQUIRED"
)

// JSON-RPC and A2A error codes (a2a-go v2.6.0 internal/jsonrpc).
const (
	codeParse              = -32700
	codeInvalidRequest     = -32600
	codeMethodNotFound     = -32601
	codeInvalidParams      = -32602
	codeTaskNotFound       = -32001
	codeTaskNotCancelable  = -32002
	codeVersionUnsupported = -32009
)

// Path is where the agent speaks JSON-RPC; its endpoint must end with it.
const Path = "/a2a"

// Entry is one audited event.
type Entry struct {
	At            time.Time  `json:"at"`
	Method        string     `json:"method"`
	MessageID     string     `json:"message_id,omitempty"`
	TaskID        string     `json:"task_id,omitempty"`
	ContextID     string     `json:"context_id,omitempty"`
	State         string     `json:"state,omitempty"`
	MessageSHA256 string     `json:"message_sha256,omitempty"`
	Until         *time.Time `json:"until,omitempty"` // a working task completes then
}

type task struct {
	id, contextID, state string
	until                *time.Time
}

// Agent is the fake A2A agent.
type Agent struct {
	tokenSum [32]byte
	cardFile string
	endpoint string
	path     string

	mu     sync.Mutex
	tasks  map[string]*task
	audit  []Entry
	failed bool
}

// New loads the durable log at dataFile (a corrupt log fails closed) and
// returns the agent. Without cardFile it serves a built-in card whose
// JSON-RPC 1.0 interface is endpoint.
func New(token, cardFile, dataFile, endpoint string) (*Agent, error) {
	if token == "" || dataFile == "" || endpoint == "" {
		return nil, errors.New("fakea2a: token, data file and endpoint are required")
	}
	a := &Agent{tokenSum: sha256.Sum256([]byte(token)), cardFile: cardFile, endpoint: endpoint, path: dataFile,
		tasks: map[string]*task{}}
	f, err := os.OpenFile(dataFile, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fakea2a: open log: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("fakea2a: sync log: %w", err)
	}
	if err := syncDir(filepath.Dir(dataFile)); err != nil {
		return nil, err
	}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var e Entry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil || e.At.IsZero() {
			return nil, errors.New("fakea2a: corrupt log")
		}
		a.apply(e)
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("fakea2a: read log: %w", err)
	}
	return a, nil
}

func (a *Agent) apply(e Entry) {
	a.audit = append(a.audit, e)
	if e.TaskID == "" || e.State == "" {
		return
	}
	t := a.tasks[e.TaskID]
	if t == nil {
		t = &task{id: e.TaskID, contextID: e.ContextID}
		a.tasks[e.TaskID] = t
	}
	t.state, t.until = e.State, e.Until
}

// record writes and syncs an entry before it takes effect. A failed write
// stops the agent answering: it never forgets what it did.
func (a *Agent) record(e Entry) error {
	if a.failed {
		return errors.New("fakea2a: log unavailable")
	}
	b, err := json.Marshal(e)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(a.path, os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			if _, err = f.Write(append(b, '\n')); err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		a.failed = true
		return err
	}
	a.apply(e)
	return nil
}

// ServeHTTP serves the card, JSON-RPC and the audit, all behind the token.
func (a *Agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="fakea2a"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/.well-known/agent-card.json":
		a.card(w)
	case r.Method == http.MethodPost && r.URL.Path == Path:
		a.rpc(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/audit":
		a.mu.Lock()
		entries := append([]Entry{}, a.audit...)
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	default:
		http.NotFound(w, r)
	}
}

// authorized compares the token's SHA-256 in constant time: only the
// digest is kept.
func (a *Agent) authorized(header string) bool {
	token, ok := bytes.CutPrefix([]byte(header), []byte("Bearer "))
	if !ok {
		return false
	}
	sum := sha256.Sum256(token)
	return subtle.ConstantTimeCompare(sum[:], a.tokenSum[:]) == 1
}

func (a *Agent) card(w http.ResponseWriter) {
	var b []byte
	if a.cardFile != "" {
		var err error
		if b, err = os.ReadFile(a.cardFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "card unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	if b == nil { // no card file (yet): the built-in card
		b, _ = json.Marshal(map[string]any{
			"name":        "Fake Procurement Agent",
			"description": "Raises purchase orders in the fake ERP",
			"version":     "1.0.0",
			"supportedInterfaces": []any{map[string]any{"url": a.endpoint, "protocolBinding": "JSONRPC",
				"protocolVersion": "1.0"}},
			"capabilities":       map[string]any{"streaming": false, "pushNotifications": false},
			"defaultInputModes":  []any{"text/plain", "application/json"},
			"defaultOutputModes": []any{"text/plain"},
			"skills": []any{map[string]any{"id": "purchase", "name": "Purchase",
				"description": "Raise a purchase order", "tags": []any{"erp", "procurement"}}},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (a *Agent) rpc(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req request
	if err != nil || json.Unmarshal(body, &req) != nil {
		reply(w, nil, nil, codeParse)
		return
	}
	if req.JSONRPC != "2.0" || len(req.ID) == 0 {
		reply(w, req.ID, nil, codeInvalidRequest)
		return
	}
	if v := r.Header.Get("A2A-Version"); v != "" && v != "1.0" {
		reply(w, req.ID, nil, codeVersionUnsupported)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var result any
	code := 0
	switch req.Method {
	case "SendMessage":
		result, code = a.sendMessage(req.Params)
	case "GetTask":
		result, code = a.getTask(req.Params)
	case "CancelTask":
		result, code = a.cancelTask(req.Params)
	default:
		code = codeMethodNotFound
	}
	if code == 0 && a.failed {
		http.Error(w, "log unavailable", http.StatusServiceUnavailable)
		return
	}
	reply(w, req.ID, result, code)
}

func reply(w http.ResponseWriter, id json.RawMessage, result any, code int) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		resp["id"] = nil
	}
	if code != 0 {
		resp["error"] = map[string]any{"code": code, "message": http.StatusText(http.StatusBadRequest)}
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *Agent) sendMessage(params json.RawMessage) (any, int) {
	var p struct {
		Message json.RawMessage `json:"message"`
	}
	var m struct {
		MessageID string `json:"messageId"`
		Role      string `json:"role"`
		Parts     []struct {
			Data json.RawMessage `json:"data"`
		} `json:"parts"`
	}
	if json.Unmarshal(params, &p) != nil || json.Unmarshal(p.Message, &m) != nil || m.MessageID == "" ||
		m.Role != "ROLE_USER" || len(m.Parts) == 0 {
		return nil, codeInvalidParams
	}
	var scenario struct {
		Scenario string `json:"scenario"`
		DelayMS  int    `json:"delay_ms"`
	}
	for _, part := range m.Parts {
		if len(part.Data) > 0 {
			_ = json.Unmarshal(part.Data, &scenario)
		}
	}
	now := time.Now().UTC()
	e := Entry{At: now, Method: "SendMessage", MessageID: m.MessageID, TaskID: "task-" + random(),
		ContextID: "ctx-" + random(), MessageSHA256: digest(p.Message)}
	switch scenario.Scenario {
	case "":
		e.State = stateCompleted
	case "working":
		delay := time.Duration(scenario.DelayMS) * time.Millisecond
		if scenario.DelayMS <= 0 {
			delay = time.Second
		}
		until := now.Add(delay)
		e.State, e.Until = stateWorking, &until
	case "hang":
		e.State = stateWorking
	case "input_required":
		e.State = stateInputRequired
	case "fail":
		e.State = stateFailed
	case "reject":
		e.State = stateRejected
	default:
		return nil, codeInvalidParams
	}
	if a.record(e) != nil {
		return nil, 0
	}
	return map[string]any{"task": a.view(a.tasks[e.TaskID])}, 0
}

func (a *Agent) lookup(params json.RawMessage) (*task, int) {
	var p struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(params, &p) != nil || p.ID == "" {
		return nil, codeInvalidParams
	}
	t := a.tasks[p.ID]
	if t == nil {
		return nil, codeTaskNotFound
	}
	// A working task with a deadline completes once it passed.
	if t.state == stateWorking && t.until != nil && !time.Now().Before(*t.until) {
		if a.record(Entry{At: time.Now().UTC(), Method: "complete", TaskID: t.id, State: stateCompleted}) != nil {
			return nil, 0
		}
	}
	return t, 0
}

func (a *Agent) getTask(params json.RawMessage) (any, int) {
	t, code := a.lookup(params)
	if t == nil {
		return nil, code
	}
	return a.view(t), 0
}

func (a *Agent) cancelTask(params json.RawMessage) (any, int) {
	t, code := a.lookup(params)
	if t == nil {
		return nil, code
	}
	switch t.state {
	case stateWorking, stateInputRequired:
	default:
		return nil, codeTaskNotCancelable
	}
	if a.record(Entry{At: time.Now().UTC(), Method: "CancelTask", TaskID: t.id, State: stateCanceled}) != nil {
		return nil, 0
	}
	return a.view(t), 0
}

func (a *Agent) view(t *task) map[string]any {
	v := map[string]any{"id": t.id, "contextId": t.contextID,
		"status": map[string]any{"state": t.state, "timestamp": time.Now().UTC().Format(time.RFC3339Nano)}}
	if t.state == stateCompleted {
		v["artifacts"] = []any{map[string]any{"artifactId": "po", "name": "purchase order",
			"parts": []any{map[string]any{"data": map[string]any{"purchase_order": "PO-" + t.id}}}}}
	}
	return v
}

func random() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// digest is the SHA-256 of the message's canonical JSON (its raw bytes when
// it is not I-JSON).
func digest(raw json.RawMessage) string {
	b, err := governance.Canonicalize(raw)
	if err != nil {
		b = raw
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
