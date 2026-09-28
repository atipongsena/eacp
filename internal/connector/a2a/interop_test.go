package a2a_test

// Interoperability with the A2A reference implementation
// (github.com/a2aproject/a2a-go/v2 v2.6.0, a test-only dependency): the
// client discovers its card and delegates to its JSON-RPC handler.

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	eacpa2a "github.com/atipongsena/eacp/internal/connector/a2a"
	"github.com/atipongsena/eacp/internal/worker"
)

// executor runs fn for each message and cancels a task on request.
type executor struct {
	fn func(*a2asrv.ExecutorContext) []a2a.Event
}

func (e executor) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for _, ev := range e.fn(ec) {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (executor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// reference serves the reference card and JSON-RPC handler behind the
// worker's bearer token, and records the JSON-RPC methods it was asked.
type reference struct {
	endpoint string
	handler  a2asrv.RequestHandler
	mu       sync.Mutex
	methods  []string
}

func newReference(t *testing.T, fn func(*a2asrv.ExecutorContext) []a2a.Event) *reference {
	t.Helper()
	r := &reference{handler: a2asrv.NewHandler(executor{fn})}
	mux := http.NewServeMux()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.Method == http.MethodPost {
			body, _ := io.ReadAll(req.Body)
			var m struct{ Method string }
			_ = json.Unmarshal(body, &m)
			r.mu.Lock()
			r.methods = append(r.methods, m.Method)
			r.mu.Unlock()
			req.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		mux.ServeHTTP(w, req)
	}))
	t.Cleanup(ts.Close)
	r.endpoint = ts.URL + "/a2a"
	card := &a2a.AgentCard{
		Name: "Reference Agent", Description: "Buys goods", Version: "2.6.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(r.endpoint, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Skills:  []a2a.AgentSkill{{ID: "buy", Name: "Buy", Description: "Buy goods", Tags: []string{"erp"}}},
		IconURL: "https://a2a.test/icon.png",
	}
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/a2a", a2asrv.NewJSONRPCHandler(r.handler))
	return r
}

func (r *reference) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.methods)
}

func (r *reference) state(t *testing.T, id string) a2a.TaskState {
	t.Helper()
	task, err := r.handler.GetTask(context.Background(), &a2a.GetTaskRequest{ID: a2a.TaskID(id)})
	if err != nil {
		t.Fatal(err)
	}
	return task.Status.State
}

func TestTheReferenceServerIsDiscovered(t *testing.T) {
	r := newReference(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := eacpa2a.New().Discover(ctx, r.endpoint, secret(t, r.endpoint, token))
	if err != nil {
		t.Fatal(err)
	}
	if d.ProtocolVersion != "1.0" || len(d.Tools) != 1 || d.Tools[0].Display != `{"iconUrl":"https://a2a.test/icon.png"}` ||
		!strings.Contains(d.Tools[0].Definition, `"name":"Reference Agent"`) ||
		string(d.ServerInfo) != `{"name":"Reference Agent","version":"2.6.0"}` {
		t.Fatalf("discovery %+v", d)
	}
}

func TestTheReferenceServerCompletesADelegation(t *testing.T) {
	var got *a2a.Message
	var mu sync.Mutex
	complete := func(ec *a2asrv.ExecutorContext) []a2a.Event {
		mu.Lock()
		got = ec.Message
		mu.Unlock()
		return []a2a.Event{a2a.NewSubmittedTask(ec, ec.Message), a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil),
			a2a.NewArtifactEvent(ec, a2a.NewTextPart("PO-42 raised")), a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil)}
	}

	t.Run("completed", func(t *testing.T) {
		r := newReference(t, complete)
		call := delegation(t, r.endpoint, `{"data":{"qty":10},"text":"Buy 10 laptops"}`)
		res := execute(t, fastClient(), call)
		if res.Outcome != worker.Succeeded || r.state(t, res.ExternalReference) != a2a.TaskStateCompleted {
			t.Fatalf("result %+v", res)
		}
		mu.Lock()
		defer mu.Unlock()
		if got.ID != call.ActionID.String() || got.Role != a2a.MessageRoleUser || len(got.Parts) != 2 ||
			got.Parts[0].Text() != "Buy 10 laptops" {
			t.Fatalf("the agent got %+v", got)
		}
		if eacp, _ := got.Metadata["eacp"].(map[string]any); eacp["operation_key"] != call.OperationKey {
			t.Fatalf("metadata %v", got.Metadata)
		}
	})

	t.Run("message reply", func(t *testing.T) {
		r := newReference(t, func(*a2asrv.ExecutorContext) []a2a.Event {
			return []a2a.Event{a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("done"))}
		})
		res := execute(t, fastClient(), delegation(t, r.endpoint, `{"text":"Buy"}`))
		if res.Outcome != worker.Succeeded || !strings.HasPrefix(res.ExternalReference, "message:") {
			t.Fatalf("result %+v", res)
		}
	})

	t.Run("auth required is cancelled", func(t *testing.T) {
		r := newReference(t, func(ec *a2asrv.ExecutorContext) []a2a.Event {
			return []a2a.Event{a2a.NewSubmittedTask(ec, ec.Message), a2a.NewStatusUpdateEvent(ec, a2a.TaskStateAuthRequired, nil)}
		})
		res := execute(t, fastClient(), delegation(t, r.endpoint, `{"text":"Buy"}`))
		if res.Outcome != worker.Ambiguous || res.ErrorClass != "a2a_auth_required" || res.RemoteReference == "" {
			t.Fatalf("result %+v", res)
		}
		// The reference answers at the first task event: the task is followed.
		if m := r.asked(); m[0] != "SendMessage" || m[len(m)-1] != "CancelTask" || slices.Index(m, "CancelTask") != len(m)-1 {
			t.Fatalf("methods %v", m)
		}
		if s := r.state(t, res.RemoteReference); s != a2a.TaskStateCanceled {
			t.Fatalf("task %s", s)
		}
	})

	t.Run("a working task is followed and cancelled", func(t *testing.T) {
		r := newReference(t, func(ec *a2asrv.ExecutorContext) []a2a.Event {
			return []a2a.Event{a2a.NewSubmittedTask(ec, ec.Message), a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil)}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		res := fastClient().Execute(ctx, delegation(t, r.endpoint, `{"text":"Buy"}`))
		if res.Outcome != worker.Ambiguous || res.ErrorClass != "a2a_interrupted" || res.RemoteReference == "" {
			t.Fatalf("result %+v", res)
		}
		m := r.asked()
		if m[0] != "SendMessage" || !slices.Contains(m, "GetTask") || m[len(m)-1] != "CancelTask" {
			t.Fatalf("methods %v", m)
		}
		if s := r.state(t, res.RemoteReference); s != a2a.TaskStateCanceled {
			t.Fatalf("task %s", s)
		}
	})
}
