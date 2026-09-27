package a2a_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/connector/a2a"
	"eacp/internal/fakeerp"
	"eacp/internal/jwttest"
	"eacp/internal/worker"
)

// rpc is one JSON-RPC request the fake agent received.
type rpc struct {
	at      time.Time
	header  http.Header
	path    string
	method  string
	id      json.RawMessage
	params  json.RawMessage
	rawBody []byte
}

// rpcAgent is a JSON-RPC agent whose answers a test scripts: reply gets
// the method and how many times it was called, and returns the JSON-RPC
// result, or a JSON-RPC error when code is not 0.
type rpcAgent struct {
	srv      *httptest.Server
	endpoint string
	mu       sync.Mutex
	calls    []rpc
	reply    func(method string, n int, params json.RawMessage) (result any, code int)
	raw      func(w http.ResponseWriter, r *http.Request) bool // true: answered
}

func newRPCAgent(t *testing.T) *rpcAgent {
	t.Helper()
	a := &rpcAgent{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		a.mu.Lock()
		a.calls = append(a.calls, rpc{at: time.Now(), header: r.Header.Clone(), path: r.URL.Path, method: req.Method,
			id: req.ID, params: req.Params, rawBody: body})
		n := 0
		for _, c := range a.calls {
			if c.method == req.Method {
				n++
			}
		}
		reply, raw := a.reply, a.raw
		a.mu.Unlock()
		if raw != nil && raw(w, r) {
			return
		}
		result, code := reply(req.Method, n, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if code != 0 {
			resp["error"] = map[string]any{"code": code, "message": "refused"}
		} else {
			resp["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(a.srv.Close)
	a.endpoint = a.srv.URL + "/a2a"
	return a
}

func (a *rpcAgent) methods() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var m []string
	for _, c := range a.calls {
		m = append(m, c.method)
	}
	return m
}

func (a *rpcAgent) call(i int) rpc {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[i]
}

func task(id, state string) map[string]any {
	return map[string]any{"id": id, "contextId": "ctx-1", "status": map[string]any{"state": state},
		"artifacts": []any{map[string]any{"artifactId": "a-1", "parts": []any{map[string]any{"text": "PO-42 raised"}}}}}
}

// sent answers SendMessage with result and every other method with task
// taskID in state.
func sent(result any, state string) func(string, int, json.RawMessage) (any, int) {
	return func(method string, _ int, _ json.RawMessage) (any, int) {
		if method == "SendMessage" {
			return result, 0
		}
		return task("task-1", state), 0
	}
}

func fastClient() *a2a.Client {
	c := a2a.New()
	c.PollStart, c.PollMax, c.CancelBudget = 20*time.Millisecond, 40*time.Millisecond, 500*time.Millisecond
	return c
}

func delegation(t *testing.T, endpoint string, payload string) worker.Call {
	t.Helper()
	tenant, action := uuid.New(), uuid.New()
	return worker.Call{TenantID: tenant, ActionID: action, OperationKey: "eacp:" + tenant.String() + ":" + action.String(),
		Attempt: 1, Generation: 1, Tool: "procurement.delegate", Endpoint: endpoint, Payload: json.RawMessage(payload),
		Contract: worker.Contract{Version: 1, SideEffects: []string{"IRREVERSIBLE_WRITE"}, IdempotencyMode: "none",
			NoEffectErrors: []string{"a2a_rejected"}, MaxAttempts: 1},
		Secret: secret(t, endpoint, token)}
}

func execute(t *testing.T, c *a2a.Client, call worker.Call) worker.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Execute(ctx, call)
}

func TestADelegationSendsOneMessage(t *testing.T) {
	for _, c := range []struct {
		name, payload string
		parts         []any
	}{
		{"text and data", `{"data":{"qty":10,"sku":"LAPTOP"},"text":"Buy 10 laptops"}`, []any{
			map[string]any{"text": "Buy 10 laptops"},
			map[string]any{"data": map[string]any{"qty": 10.0, "sku": "LAPTOP"}, "mediaType": "application/json"}}},
		{"text", `{"text":"Buy 10 laptops"}`, []any{map[string]any{"text": "Buy 10 laptops"}}},
		{"data", `{"data":{"qty":10}}`, []any{map[string]any{"data": map[string]any{"qty": 10.0}, "mediaType": "application/json"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newRPCAgent(t)
			a.reply = sent(map[string]any{"task": task("task-1", "TASK_STATE_COMPLETED")}, "")
			call := delegation(t, a.endpoint, c.payload)
			if res := execute(t, fastClient(), call); res != (worker.Result{Outcome: worker.Succeeded, ExternalReference: "task-1"}) {
				t.Fatalf("result %+v", res)
			}
			if m := a.methods(); !slices.Equal(m, []string{"SendMessage"}) {
				t.Fatalf("methods %v", m)
			}
			r := a.call(0)
			for h, want := range map[string]string{"Content-Type": "application/json", "Accept": "application/json",
				"A2A-Version": "1.0", "Authorization": "Bearer " + token} {
				if got := r.header.Get(h); got != want {
					t.Errorf("%s = %q, want %q", h, got, want)
				}
			}
			var body map[string]any
			if err := json.Unmarshal(r.rawBody, &body); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"jsonrpc": "2.0", "id": "1", "method": "SendMessage", "params": map[string]any{
				"message": map[string]any{"messageId": call.ActionID.String(), "role": "ROLE_USER", "parts": c.parts,
					"metadata": map[string]any{"eacp": map[string]any{"operation_key": call.OperationKey}}},
				"configuration": map[string]any{"returnImmediately": false}}}
			if !reflect.DeepEqual(body, want) || r.path != "/a2a" {
				t.Fatalf("POST %s\n%s\nwant %v", r.path, r.rawBody, want)
			}
		})
	}
}

func TestEveryOutcomeIsClassified(t *testing.T) {
	status := func(code int) func(http.ResponseWriter, *http.Request) bool {
		return func(w http.ResponseWriter, _ *http.Request) bool { w.WriteHeader(code); return true }
	}
	body := func(s string) func(http.ResponseWriter, *http.Request) bool {
		return func(w http.ResponseWriter, _ *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(s))
			return true
		}
	}
	for _, c := range []struct {
		name    string
		result  any
		code    int
		raw     func(http.ResponseWriter, *http.Request) bool
		want    worker.Result
		cancels bool
	}{
		{name: "message reply", result: map[string]any{"message": map[string]any{"messageId": "m-1", "role": "ROLE_AGENT",
			"parts": []any{map[string]any{"text": "done"}}}},
			want: worker.Result{Outcome: worker.Succeeded, ExternalReference: "message:m-1"}},
		{name: "completed", result: map[string]any{"task": task("task-1", "TASK_STATE_COMPLETED")},
			want: worker.Result{Outcome: worker.Succeeded, ExternalReference: "task-1"}},
		{name: "rejected", result: map[string]any{"task": task("task-1", "TASK_STATE_REJECTED")},
			want: worker.Result{Outcome: worker.NoEffect, ErrorClass: "a2a_rejected", RemoteReference: "task-1"}},
		{name: "failed", result: map[string]any{"task": task("task-1", "TASK_STATE_FAILED")},
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_failed", RemoteReference: "task-1"}},
		{name: "canceled", result: map[string]any{"task": task("task-1", "TASK_STATE_CANCELED")},
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_canceled", RemoteReference: "task-1"}},
		{name: "input required", result: map[string]any{"task": task("task-1", "TASK_STATE_INPUT_REQUIRED")}, cancels: true,
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_input_required", RemoteReference: "task-1"}},
		{name: "auth required", result: map[string]any{"task": task("task-1", "TASK_STATE_AUTH_REQUIRED")}, cancels: true,
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_auth_required", RemoteReference: "task-1"}},
		{name: "unknown state", result: map[string]any{"task": task("task-1", "TASK_STATE_PAUSED")}, cancels: true,
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response", RemoteReference: "task-1"}},
		{name: "JSON-RPC error", code: -32001, want: worker.Result{Outcome: worker.NoEffect, ErrorClass: "a2a_rpc_32001"}},
		{name: "JSON-RPC error out of range", code: -1234567, want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
		{name: "HTTP 401", raw: status(http.StatusUnauthorized), want: worker.Result{Outcome: worker.NoEffect, ErrorClass: "unauthorized"}},
		{name: "HTTP 403", raw: status(http.StatusForbidden), want: worker.Result{Outcome: worker.NoEffect, ErrorClass: "unauthorized"}},
		{name: "HTTP 500", raw: status(http.StatusInternalServerError), want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "http_500"}},
		{name: "not JSON", raw: body(`<html>`), want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
		{name: "not JSON-RPC 2.0", raw: body(`{"jsonrpc":"1.0","id":"1","result":{"task":{"id":"t","status":{"state":"TASK_STATE_COMPLETED"}}}}`),
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
		{name: "another id", raw: body(`{"jsonrpc":"2.0","id":"7","result":{"task":{"id":"t","status":{"state":"TASK_STATE_COMPLETED"}}}}`),
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
		{name: "task and message", result: map[string]any{"task": task("task-1", "TASK_STATE_COMPLETED"),
			"message": map[string]any{"messageId": "m-1", "role": "ROLE_AGENT", "parts": []any{}}},
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
		{name: "neither", result: map[string]any{}, want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
		{name: "a task id with a space", result: map[string]any{"task": task("task 1", "TASK_STATE_COMPLETED")},
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
		{name: "a task id of 257", result: map[string]any{"task": task(strings.Repeat("t", 257), "TASK_STATE_COMPLETED")},
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
		{name: "a message id with a slash", result: map[string]any{"message": map[string]any{"messageId": "m/1",
			"role": "ROLE_AGENT", "parts": []any{}}},
			want: worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newRPCAgent(t)
			a.raw = func(w http.ResponseWriter, r *http.Request) bool {
				if c.raw != nil && len(a.methods()) == 1 {
					return c.raw(w, r)
				}
				return false
			}
			a.reply = func(method string, _ int, _ json.RawMessage) (any, int) {
				if method == "SendMessage" {
					return c.result, c.code
				}
				return task("task-1", "TASK_STATE_CANCELED"), 0
			}
			if res := execute(t, fastClient(), delegation(t, a.endpoint, `{"text":"Buy"}`)); res != c.want {
				t.Fatalf("result %+v, want %+v", res, c.want)
			}
			want := []string{"SendMessage"}
			if c.cancels {
				want = append(want, "CancelTask")
			}
			if m := a.methods(); !slices.Equal(m, want) {
				t.Fatalf("methods %v, want %v", m, want)
			}
		})
	}

	t.Run("connection refused", func(t *testing.T) {
		a := newRPCAgent(t)
		a.srv.Close()
		res := execute(t, fastClient(), delegation(t, a.endpoint, `{"text":"Buy"}`))
		if res != (worker.Result{Outcome: worker.NoEffect, ErrorClass: "connection_refused_before_send"}) {
			t.Fatalf("result %+v", res)
		}
	})
}

func TestAWorkingTaskIsFollowed(t *testing.T) {
	a := newRPCAgent(t)
	a.reply = func(method string, n int, _ json.RawMessage) (any, int) {
		switch {
		case method == "SendMessage":
			return map[string]any{"task": task("task-1", "TASK_STATE_SUBMITTED")}, 0
		case method == "GetTask" && n < 3:
			return task("task-1", "TASK_STATE_WORKING"), 0
		default:
			return task("task-1", "TASK_STATE_COMPLETED"), 0
		}
	}
	res := execute(t, fastClient(), delegation(t, a.endpoint, `{"text":"Buy"}`))
	if res != (worker.Result{Outcome: worker.Succeeded, ExternalReference: "task-1"}) {
		t.Fatalf("result %+v", res)
	}
	if m := a.methods(); !slices.Equal(m, []string{"SendMessage", "GetTask", "GetTask", "GetTask"}) {
		t.Fatalf("methods %v", m)
	}
	var gaps []time.Duration
	for i := 1; i < 4; i++ {
		gaps = append(gaps, a.call(i).at.Sub(a.call(i-1).at))
		var p map[string]any
		if err := json.Unmarshal(a.call(i).params, &p); err != nil || !reflect.DeepEqual(p, map[string]any{"id": "task-1"}) {
			t.Fatalf("GetTask params %s", a.call(i).params)
		}
		if id := string(a.call(i).id); id != fmt.Sprintf(`"%d"`, i+1) {
			t.Fatalf("GetTask id %s", id)
		}
	}
	// 20 ms, 40 ms, then capped at 40 ms.
	if gaps[0] < 20*time.Millisecond || gaps[1] < 40*time.Millisecond || gaps[2] < 40*time.Millisecond {
		t.Fatalf("poll gaps %v", gaps)
	}
}

func TestAMismatchedTaskIsRefused(t *testing.T) {
	a := newRPCAgent(t)
	a.reply = func(method string, _ int, _ json.RawMessage) (any, int) {
		switch method {
		case "SendMessage":
			return map[string]any{"task": task("task-1", "TASK_STATE_WORKING")}, 0
		case "GetTask":
			return task("task-2", "TASK_STATE_COMPLETED"), 0
		}
		return task("task-1", "TASK_STATE_CANCELED"), 0
	}
	res := execute(t, fastClient(), delegation(t, a.endpoint, `{"text":"Buy"}`))
	if res != (worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response", RemoteReference: "task-1"}) {
		t.Fatalf("result %+v", res)
	}
	if m := a.methods(); !slices.Equal(m, []string{"SendMessage", "GetTask", "CancelTask"}) {
		t.Fatalf("methods %v", m)
	}
	var p map[string]any
	_ = json.Unmarshal(a.call(2).params, &p)
	if p["id"] != "task-1" {
		t.Fatalf("cancelled %v", p)
	}
}

func TestAnInterruptedTaskIsCancelledOnce(t *testing.T) {
	working := func(slowCancel bool) func(string, int, json.RawMessage) (any, int) {
		return func(method string, _ int, _ json.RawMessage) (any, int) {
			switch method {
			case "SendMessage":
				return map[string]any{"task": task("task-1", "TASK_STATE_WORKING")}, 0
			case "CancelTask":
				if slowCancel {
					time.Sleep(2 * time.Second)
				}
				return task("task-1", "TASK_STATE_CANCELED"), 0
			}
			return task("task-1", "TASK_STATE_WORKING"), 0
		}
	}
	for _, c := range []struct {
		name       string
		ctx        func() (context.Context, context.CancelFunc)
		slowCancel bool
	}{
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 150*time.Millisecond)
		}, false},
		{"cancelled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(150*time.Millisecond, cancel)
			return ctx, cancel
		}, false},
		{"a cancel that hangs", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 150*time.Millisecond)
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newRPCAgent(t)
			a.reply = working(c.slowCancel)
			cl := fastClient()
			cl.CancelBudget = 200 * time.Millisecond
			ctx, cancel := c.ctx()
			defer cancel()
			start := time.Now()
			res := cl.Execute(ctx, delegation(t, a.endpoint, `{"text":"Buy"}`))
			if res != (worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_interrupted", RemoteReference: "task-1"}) {
				t.Fatalf("result %+v", res)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("Execute took %v: the cancel has its own short budget", elapsed)
			}
			m := a.methods()
			if m[len(m)-1] != "CancelTask" || slices.Index(m, "CancelTask") != len(m)-1 {
				t.Fatalf("methods %v, want one CancelTask last", m)
			}
		})
	}

	t.Run("no task yet", func(t *testing.T) {
		a := newRPCAgent(t)
		a.raw = func(w http.ResponseWriter, r *http.Request) bool {
			<-r.Context().Done() // SendMessage never answers
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		res := fastClient().Execute(ctx, delegation(t, a.endpoint, `{"text":"Buy"}`))
		if res != (worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout"}) {
			t.Fatalf("result %+v", res)
		}
		if m := a.methods(); !slices.Equal(m, []string{"SendMessage"}) {
			t.Fatalf("methods %v", m)
		}
	})
}

func TestAnInvalidPayloadSendsNothing(t *testing.T) {
	for _, p := range []string{`{}`, `{"text":""}`, `{"text":5}`, `{"data":[]}`, `{"data":"x"}`, `{"data":null}`,
		`{"text":"x","other":1}`, `{"text":"` + strings.Repeat("é", 65537) + `"}`, `[]`, `"text"`, `not json`} {
		a := newRPCAgent(t)
		a.reply = sent(map[string]any{"task": task("task-1", "TASK_STATE_COMPLETED")}, "")
		label := p
		if len(label) > 40 {
			label = label[:40]
		}
		if res := execute(t, fastClient(), delegation(t, a.endpoint, p)); res != (worker.Result{Outcome: worker.NoEffect,
			ErrorClass: "invalid_payload"}) {
			t.Fatalf("%s: result %+v", label, res)
		}
		if m := a.methods(); len(m) != 0 {
			t.Fatalf("%s: sent %v", label, m)
		}
	}
	a := newRPCAgent(t)
	a.reply = sent(map[string]any{"task": task("task-1", "TASK_STATE_COMPLETED")}, "")
	if res := execute(t, fastClient(), delegation(t, a.endpoint, `{"text":"`+strings.Repeat("é", 65536)+`"}`)); res.Outcome != worker.Succeeded {
		t.Fatalf("65 536 characters: %+v", res)
	}
}

func TestResponsesAreBounded(t *testing.T) {
	a := newRPCAgent(t)
	a.reply = sent(map[string]any{"task": map[string]any{"id": "task-1", "contextId": "c",
		"status": map[string]any{"state": "TASK_STATE_COMPLETED"}, "metadata": map[string]any{"x": strings.Repeat("x", 1<<20)}}}, "")
	res := execute(t, fastClient(), delegation(t, a.endpoint, `{"text":"Buy"}`))
	if res != (worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}) {
		t.Fatalf("result %+v", res)
	}
}

func TestNoRedirectIsFollowed(t *testing.T) {
	a := newRPCAgent(t)
	a.raw = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/a2a" {
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
			return true
		}
		return false
	}
	a.reply = sent(map[string]any{"task": task("task-1", "TASK_STATE_COMPLETED")}, "")
	res := execute(t, fastClient(), delegation(t, a.endpoint, `{"text":"Buy"}`))
	if res != (worker.Result{Outcome: worker.Ambiguous, ErrorClass: "http_307"}) {
		t.Fatalf("result %+v", res)
	}
	if n := len(a.methods()); n != 1 {
		t.Fatalf("%d requests: the redirect was followed", n)
	}
}

func TestTheCredentialNeverLeavesTheHost(t *testing.T) {
	a := newRPCAgent(t)
	// A careless agent echoes the credential as its task id.
	a.reply = sent(map[string]any{"task": task("t-"+token, "TASK_STATE_FAILED")}, "")
	res := execute(t, fastClient(), delegation(t, a.endpoint, `{"text":"Buy"}`))
	if strings.Contains(fmt.Sprintf("%+v", res), token) || res.Outcome != worker.Ambiguous {
		t.Fatalf("result %+v", res)
	}
	for i := range a.methods() {
		if strings.Contains(string(a.call(i).rawBody), token) {
			t.Fatal("the credential is in a request body")
		}
	}
	if a2a.New().Lookup(context.Background(), worker.LookupCall{}).Status != worker.LookupUnknown {
		t.Fatal("an A2A lookup must be unknown")
	}
}

// TestADelegationIsSignedWithSigV4: an agent behind AWS IAM gets every
// header of the message signed, A2A-Version included.
func TestADelegationIsSignedWithSigV4(t *testing.T) {
	const keyID, secretKey, session = "ASIAA2ATEST000000001", "a2a-aws-secret", "a2a-aws-session"
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>`+
			`<AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken>`+
			`<Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`,
			keyID, secretKey, session, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer sts.Close()
	var signed []string
	var verr error
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, verr = fakeerp.VerifySigV4(r, body, "us-east-1", "execute-api", time.Now(), func(id string) (fakeerp.SigV4Key, bool) {
			return fakeerp.SigV4Key{SecretKey: secretKey, SessionToken: session}, id == keyID
		})
		auth := r.Header.Get("Authorization")
		if parts := strings.SplitN(auth, "SignedHeaders=", 2); len(parts) == 2 {
			signed = strings.Split(strings.SplitN(parts[1], ",", 2)[0], ";")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"task":{"id":"task-1","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"}}}}`))
	}))
	defer agent.Close()
	subject := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(subject, []byte(jwttest.New(t).Sign(map[string]any{"sub": "worker", "aud": "sts",
		"exp": time.Now().Add(time.Hour).Unix()})), 0o600); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	p := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(p, []byte(fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"a2a","host":%q,
		"aws":{"role_arn":"arn:aws:iam::123456789012:role/eacp","region":"us-east-1","service":"execute-api",
		"sts_endpoint":%q,"subject_token":{"file":%q}}}]}`, tenant, strings.TrimPrefix(agent.URL, "http://"), sts.URL, subject)),
		0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(p, worker.AllowPlainTokenURL())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := agent.URL + "/a2a"
	s, err := store.Credential(context.Background(), tenant, "a2a", endpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := delegation(t, endpoint, `{"text":"Buy"}`)
	call.TenantID, call.Secret = tenant, s
	if res := execute(t, fastClient(), call); res.Outcome != worker.Succeeded || verr != nil {
		t.Fatalf("result %+v, verification %v", res, verr)
	}
	for _, h := range []string{"a2a-version", "accept", "content-type", "x-amz-security-token"} {
		if !slices.Contains(signed, h) {
			t.Errorf("%s is not signed: %v", h, signed)
		}
	}
}
