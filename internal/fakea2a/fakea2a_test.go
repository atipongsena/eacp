package fakea2a_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/connector/a2a"
	"github.com/atipongsena/eacp/internal/fakea2a"
	"github.com/atipongsena/eacp/internal/worker"
)

const token = "fakea2a-test-token"

type fake struct {
	t        *testing.T
	srv      *httptest.Server
	endpoint string
	data     string
}

func start(t *testing.T, cardFile, data string) *fake {
	t.Helper()
	f := &fake{t: t, data: data}
	var h http.Handler
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(f.srv.Close)
	f.endpoint = f.srv.URL + "/a2a"
	var err error
	if h, err = fakea2a.New(token, cardFile, data, f.endpoint); err != nil {
		t.Fatal(err)
	}
	return f
}

func newFake(t *testing.T) *fake {
	return start(t, "", filepath.Join(t.TempDir(), "a2a.log"))
}

type rpcReply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

type taskView struct {
	ID        string `json:"id"`
	ContextID string `json:"contextId"`
	Status    struct {
		State string `json:"state"`
	} `json:"status"`
	Artifacts []json.RawMessage `json:"artifacts"`
}

func (f *fake) rpc(method string, params any) rpcReply {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "7", "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, f.endpoint, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var r rpcReply
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&r) != nil || r.ID != "7" {
		f.t.Fatalf("%s: HTTP %d, reply %+v", method, resp.StatusCode, r)
	}
	return r
}

func (f *fake) send(messageID string, data map[string]any) taskView {
	f.t.Helper()
	parts := []any{map[string]any{"text": "Buy 10 laptops"}}
	if data != nil {
		parts = append(parts, map[string]any{"data": data, "mediaType": "application/json"})
	}
	r := f.rpc("SendMessage", map[string]any{"message": map[string]any{"messageId": messageID, "role": "ROLE_USER",
		"parts": parts}, "configuration": map[string]any{"returnImmediately": true}})
	var res struct {
		Task *taskView `json:"task"`
	}
	if r.Error != nil || json.Unmarshal(r.Result, &res) != nil || res.Task == nil {
		f.t.Fatalf("SendMessage: %+v %s", r.Error, r.Result)
	}
	return *res.Task
}

func (f *fake) task(method, id string) (taskView, int) {
	f.t.Helper()
	r := f.rpc(method, map[string]any{"id": id})
	if r.Error != nil {
		return taskView{}, r.Error.Code
	}
	var v taskView
	if err := json.Unmarshal(r.Result, &v); err != nil {
		f.t.Fatal(err)
	}
	return v, 0
}

type entry struct {
	Method        string `json:"method"`
	MessageID     string `json:"message_id"`
	TaskID        string `json:"task_id"`
	State         string `json:"state"`
	MessageSHA256 string `json:"message_sha256"`
}

func (f *fake) audit() []entry {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var entries []entry
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&entries) != nil {
		f.t.Fatalf("audit: HTTP %d", resp.StatusCode)
	}
	return entries
}

func TestTheFakeAgentServesEveryScenario(t *testing.T) {
	f := newFake(t)

	// The built-in card passes the scanner's checks.
	store := secretFor(t, f.endpoint)
	d, err := a2a.New().Discover(context.Background(), f.endpoint, store)
	if err != nil || len(d.Tools) != 1 || !strings.Contains(d.Tools[0].Definition, `"id":"purchase"`) {
		t.Fatalf("discovery %+v, %v", d, err)
	}

	for _, c := range []struct {
		scenario string
		state    string
	}{
		{"", "TASK_STATE_COMPLETED"},
		{"input_required", "TASK_STATE_INPUT_REQUIRED"},
		{"fail", "TASK_STATE_FAILED"},
		{"reject", "TASK_STATE_REJECTED"},
		{"hang", "TASK_STATE_WORKING"},
	} {
		var data map[string]any
		if c.scenario != "" {
			data = map[string]any{"scenario": c.scenario}
		}
		v := f.send(uuid.NewString(), data)
		if v.Status.State != c.state || v.ID == "" || v.ContextID == "" {
			t.Fatalf("%q: task %+v", c.scenario, v)
		}
		if c.scenario == "" && len(v.Artifacts) != 1 {
			t.Fatalf("a completed task has %d artifacts", len(v.Artifacts))
		}
	}

	// working: WORKING until delay_ms, then COMPLETED.
	w := f.send(uuid.NewString(), map[string]any{"scenario": "working", "delay_ms": 100})
	if got, _ := f.task("GetTask", w.ID); w.Status.State != "TASK_STATE_WORKING" || got.Status.State != "TASK_STATE_WORKING" {
		t.Fatalf("working: %s then %s", w.Status.State, got.Status.State)
	}
	time.Sleep(150 * time.Millisecond)
	if got, _ := f.task("GetTask", w.ID); got.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("working after the delay: %s", got.Status.State)
	}

	// hang: never finishes; CancelTask cancels it once.
	h := f.send("msg-hang", map[string]any{"scenario": "hang"})
	if got, _ := f.task("CancelTask", h.ID); got.Status.State != "TASK_STATE_CANCELED" {
		t.Fatalf("cancelled hang: %s", got.Status.State)
	}
	if _, code := f.task("CancelTask", h.ID); code != -32002 {
		t.Fatalf("a second cancel: code %d", code)
	}
	if _, code := f.task("GetTask", "task-unknown"); code != -32001 {
		t.Fatalf("an unknown task: code %d", code)
	}
	if r := f.rpc("ListTasks", map[string]any{}); r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("an unsupported method: %+v", r)
	}

	// The audit names every message and task, never their content.
	entries := f.audit()
	var sendHang, cancelHang bool
	for _, e := range entries {
		if e.Method == "SendMessage" && e.MessageID == "msg-hang" && e.TaskID == h.ID && e.State == "TASK_STATE_WORKING" &&
			len(e.MessageSHA256) == 64 {
			sendHang = true
		}
		if e.Method == "CancelTask" && e.TaskID == h.ID && e.State == "TASK_STATE_CANCELED" {
			cancelHang = true
		}
	}
	if !sendHang || !cancelHang {
		t.Fatalf("audit %+v", entries)
	}
	b, _ := json.Marshal(entries)
	if strings.Contains(string(b), "laptops") {
		t.Fatal("the audit holds message content")
	}
}

func TestTheFakeAgentNeedsItsToken(t *testing.T) {
	f := newFake(t)
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, a2a.CardPath}, {http.MethodPost, "/a2a"}, {http.MethodGet, "/v1/audit"},
	} {
		for _, auth := range []string{"", "Bearer wrong", "Bearer " + token + "x"} {
			req, _ := http.NewRequest(r.method, f.srv.URL+r.path, strings.NewReader(`{}`))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s with %q: HTTP %d", r.method, r.path, auth, resp.StatusCode)
			}
		}
	}
}

func TestTheFakeAgentsLogSurvivesARestart(t *testing.T) {
	data := filepath.Join(t.TempDir(), "a2a.log")
	f := start(t, "", data)
	h := f.send("msg-1", map[string]any{"scenario": "hang"})
	f.send("msg-2", nil)

	g := start(t, "", data)
	if got, _ := g.task("GetTask", h.ID); got.Status.State != "TASK_STATE_WORKING" {
		t.Fatalf("after a restart: %s", got.Status.State)
	}
	if got, _ := g.task("CancelTask", h.ID); got.Status.State != "TASK_STATE_CANCELED" {
		t.Fatalf("cancel after a restart: %s", got.Status.State)
	}
	var sends int
	for _, e := range g.audit() {
		if e.Method == "SendMessage" {
			sends++
		}
	}
	if sends != 2 {
		t.Fatalf("%d messages after a restart", sends)
	}

	if err := os.WriteFile(data, []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fakea2a.New(token, "", data, "http://fakea2a.test/a2a"); err == nil {
		t.Fatal("a corrupt log was accepted")
	}
}

func TestTheCardFileIsReReadForDrift(t *testing.T) {
	cardFile := filepath.Join(t.TempDir(), "card.json")
	f := start(t, cardFile, filepath.Join(t.TempDir(), "a2a.log"))
	get := func() string {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+a2a.CardPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var c struct{ Name string }
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" ||
			json.NewDecoder(resp.Body).Decode(&c) != nil {
			t.Fatalf("card: HTTP %d", resp.StatusCode)
		}
		return c.Name
	}
	// Until the file exists, the built-in card is served.
	if n := get(); n != "Fake Procurement Agent" {
		t.Fatalf("card without its file %q", n)
	}
	if err := os.WriteFile(cardFile, []byte(`{"name":"v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := get(); n != "v1" {
		t.Fatalf("card %q", n)
	}
	if err := os.WriteFile(cardFile, []byte(`{"name":"v2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := get(); n != "v2" {
		t.Fatalf("card after the change %q", n)
	}
}

func secretFor(t *testing.T, endpoint string) worker.Secret {
	t.Helper()
	tenant := uuid.New()
	host := strings.TrimPrefix(endpoint, "http://")
	host, _, _ = strings.Cut(host, "/")
	p := filepath.Join(t.TempDir(), "secrets.json")
	b, _ := json.Marshal(map[string]any{"secrets": []any{map[string]any{"tenant_id": tenant, "secret_ref": "a2a",
		"host": host, "value": token}}})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(p)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Resolve(tenant, "a2a", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
