package studioruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	// maxWaitSeconds bounds one wait on an action, so a revoked key or a
	// replaced version is noticed within it.
	maxWaitSeconds = 5
	// maxAnswer is the largest answer PostgreSQL keeps.
	maxAnswer = 65536
	// finishTimeout bounds the last call of a run, made after its deadline.
	finishTimeout = 10 * time.Second
)

var runtimeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// errLost is a lease another runtime took over: this one stops without
// finishing the run.
var errLost = errors.New("studioruntime: the run's lease was lost")

// errKilled is a run a kill matched: PostgreSQL failed it killed at its
// heartbeat, so this runtime stops without finishing it (Phase 28).
var errKilled = errors.New("studioruntime: the run was killed")

// Options configures a Runtime.
type Options struct {
	// API is the EACP API origin, such as http://controlplane-api:8080.
	API string
	// Gateway is the optional LLM gateway origin. V1 needs only the API.
	Gateway string
	// Keys are the runtime's own principal keys, one per tenant.
	Keys map[uuid.UUID]string
	// Master derives every agent key (ADR-033 §2).
	Master *Master
	// ID names this replica in run leases.
	ID string
	// Lease is how long a claim holds a run (default 30 s); it is renewed
	// every third of it.
	Lease time.Duration
	// Concurrency is how many runs one RunOnce drives at most (default 4).
	Concurrency int
	// Poll is how long Run waits when there was nothing to claim (default 1 s).
	Poll time.Duration
	// Log never receives a key, the master, an input, an output or an answer.
	Log *slog.Logger
	// Redact is given the master and every key before it is used (the
	// service's logging.SecretSet).
	Redact func(values ...string)
}

// Runtime runs Studio agents.
type Runtime struct {
	api         *client
	gateway     *client
	keys        map[uuid.UUID]string
	tenants     []uuid.UUID
	master      *Master
	id          string
	lease       time.Duration
	concurrency int
	poll        time.Duration
	log         *slog.Logger
	redact      func(values ...string)
}

// New checks o and returns a Runtime.
func New(o Options) (*Runtime, error) {
	u, err := url.Parse(o.API)
	switch {
	case o.API == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return nil, errors.New("studioruntime: the API is an http:// or https:// origin")
	case len(o.Keys) == 0:
		return nil, errors.New("studioruntime: no runtime key")
	case o.Master == nil:
		return nil, errors.New("studioruntime: no master secret")
	case !runtimeID.MatchString(o.ID):
		return nil, errors.New("studioruntime: the runtime id is 1-128 characters of [A-Za-z0-9._:-]")
	}
	r := &Runtime{api: newClient(strings.TrimSuffix(o.API, "/")), keys: o.Keys, master: o.Master, id: o.ID,
		lease: o.Lease, concurrency: o.Concurrency, poll: o.Poll, log: o.Log, redact: o.Redact}
	if o.Gateway != "" {
		u, err := url.Parse(o.Gateway)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return nil, errors.New("studioruntime: the gateway is an http:// or https:// origin")
		}
		r.gateway = newClient(strings.TrimSuffix(o.Gateway, "/"))
		r.gateway.http.Timeout = 0 // The run context bounds long provider calls.
	}
	if r.lease <= 0 {
		r.lease = 30 * time.Second
	}
	if r.lease < 5*time.Second || r.lease > 5*time.Minute {
		return nil, errors.New("studioruntime: a lease is 5 s to 5 min")
	}
	if r.concurrency <= 0 {
		r.concurrency = 4
	}
	if r.poll <= 0 {
		r.poll = time.Second
	}
	if r.log == nil {
		r.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if r.redact == nil {
		r.redact = func(...string) {}
	}
	r.redact(o.Master.Redactions()...)
	for t, k := range o.Keys {
		r.redact(k)
		r.tenants = append(r.tenants, t)
	}
	slices.SortFunc(r.tenants, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return r, nil
}

// Run claims and drives runs until ctx ends.
func (r *Runtime) Run(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.WarnContext(ctx, "studio runs", "err", err)
		}
		if n == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(r.poll):
			}
		}
	}
}

// claim is one run eacp.studio_run_claim leased to this runtime.
type claim struct {
	ID           uuid.UUID         `json:"id"`
	VersionID    uuid.UUID         `json:"version_id"`
	Generation   int64             `json:"generation"`
	Deadline     time.Time         `json:"deadline"`
	Inputs       map[string]string `json:"inputs"`
	Definition   definition        `json:"definition"`
	Subject      string            `json:"subject"`
	Steps        []recorded        `json:"steps"`
	CredentialID *uuid.UUID        `json:"credential_id"`
	Credential   string            `json:"credential"`
	Mode         string            `json:"mode"`
	CurrentIndex int               `json:"current_index"`
	Nodes        []node            `json:"nodes"`
}

type recorded struct {
	Index    int       `json:"index"`
	ActionID uuid.UUID `json:"action_id"`
}

// definition is what the runtime reads of a saved definition; PostgreSQL
// validated all of it (migration 00027).
type definition struct {
	SchemaVersion int    `json:"schema_version"`
	Steps         []step `json:"steps"`
}

type step struct {
	ID                string          `json:"id"`
	Kind              string          `json:"kind"`
	Tool              string          `json:"tool"`
	ToolSchemaVersion string          `json:"tool_schema_version"`
	Operation         string          `json:"operation"`
	Target            string          `json:"target"`
	Resource          string          `json:"resource"`
	Payload           json.RawMessage `json:"payload"`
	Text              string          `json:"text"`
	Next              string          `json:"next"`
	Then              string          `json:"then"`
	Else              string          `json:"else"`
	Model             string          `json:"model"`
	Instruction       string          `json:"instruction"`
	Input             json.RawMessage `json:"input"`
	MaxOutputTokens   int64           `json:"max_output_tokens"`
	Condition         struct {
		Left     json.RawMessage `json:"left"`
		Operator string          `json:"operator"`
		Right    json.RawMessage `json:"right"`
	} `json:"condition"`
}

// RunOnce claims up to Concurrency runs across its tenants and drives each
// to its end. It returns how many it claimed.
func (r *Runtime) RunOnce(ctx context.Context) (int, error) {
	type job struct {
		tenant uuid.UUID
		c      claim
	}
	var jobs []job
	var errs []error
	for _, tenant := range r.tenants {
		limit := r.concurrency - len(jobs)
		if limit <= 0 {
			break
		}
		var out struct {
			Runs []claim `json:"runs"`
		}
		_, err := r.api.do(ctx, r.keys[tenant], "POST", "/v1/studio/runtime/claims", nil, map[string]any{
			"runtime_id": r.id, "master_version": r.master.Version(),
			"lease_seconds": int(r.lease / time.Second), "limit": limit}, &out)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: claim: %w", tenant, err))
			continue
		}
		for _, c := range out.Runs {
			jobs = append(jobs, job{tenant, c})
		}
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.execute(ctx, j.tenant, j.c)
		}()
	}
	wg.Wait()
	return len(jobs), errors.Join(errs...)
}

// execution is one run being driven.
type execution struct {
	r        *Runtime
	tenant   uuid.UUID
	c        claim
	key      string // the agent key, once derived
	replaced atomic.Bool
	killed   atomic.Bool
}

// execute drives c and finishes it, unless its lease was lost or ctx ended.
func (r *Runtime) execute(parent context.Context, tenant uuid.UUID, c claim) {
	x := &execution{r: r, tenant: tenant, c: c}
	ctx, cancel := context.WithDeadline(parent, c.Deadline)
	defer cancel()
	lost := make(chan struct{})
	beats, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		x.heartbeats(beats, cancel, lost)
	}()
	answer, reason, err := x.steps(ctx)
	stop()
	wg.Wait()
	select {
	case <-lost:
		err = errLost
	default:
	}
	switch {
	case x.killed.Load():
		r.log.InfoContext(parent, "studio run killed", "run", c.ID.String())
		return
	case errors.Is(err, errLost):
		r.log.WarnContext(parent, "studio run lease lost", "run", c.ID.String())
		return
	case parent.Err() != nil:
		return // shutting down: the lease lapses and another claim resumes the run
	case ctx.Err() != nil && reason == "":
		reason, err = "deadline_exceeded", nil
	case err != nil:
		// The API kept failing: the lease lapses and a later claim resumes.
		r.log.WarnContext(parent, "studio run interrupted", "run", c.ID.String(), "err", err)
		return
	}
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(parent), finishTimeout)
	defer fcancel()
	body := map[string]any{"runtime_id": r.id, "generation": c.Generation}
	if reason == "" {
		body["state"], body["answer"] = "SUCCEEDED", answer
	} else {
		body["state"], body["reason"] = "FAILED", reason
	}
	if _, err := r.api.do(fctx, r.keys[tenant], "POST", "/v1/studio/runtime/runs/"+c.ID.String()+"/finish",
		nil, body, nil); err != nil {
		r.log.WarnContext(parent, "studio run finish", "run", c.ID.String(), "err", err)
		return
	}
	r.log.InfoContext(parent, "studio run finished", "run", c.ID.String(), "state", body["state"], "reason", reason)
}

// heartbeats renews the lease every third of it until ctx ends; a lost lease
// or a kill cancels the run, a replaced version marks it.
func (x *execution) heartbeats(ctx context.Context, cancel context.CancelFunc, lost chan struct{}) {
	t := time.NewTicker(x.r.lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := x.beat(ctx)
		switch {
		case errors.Is(err, errKilled):
			cancel()
			return
		case errors.Is(err, errLost):
			close(lost)
			cancel()
			return
		}
	}
}

// beat renews the lease and reports whether the version is still ACTIVE.
// A killed run is errKilled.
func (x *execution) beat(ctx context.Context) (bool, error) {
	if x.killed.Load() {
		return false, errKilled
	}
	var out struct {
		VersionActive bool `json:"version_active"`
		Killed        bool `json:"killed"`
	}
	status, err := x.r.api.do(ctx, x.r.keys[x.tenant], "POST", "/v1/studio/runtime/runs/"+x.c.ID.String()+"/heartbeat",
		nil, map[string]any{"runtime_id": x.r.id, "generation": x.c.Generation,
			"lease_seconds": int(x.r.lease / time.Second)}, &out)
	if status == 403 {
		return false, errLost
	}
	if err != nil {
		return false, err
	}
	if out.Killed {
		x.killed.Store(true)
		return false, errKilled
	}
	if !out.VersionActive {
		x.replaced.Store(true)
	}
	return out.VersionActive, nil
}

// steps runs the definition and returns the answer, or a failure reason.
func (x *execution) steps(ctx context.Context) (answer, reason string, err error) {
	c := x.c
	if c.Credential != "ok" || c.CredentialID == nil {
		switch c.Credential {
		case "expired":
			return "", "credential_expired", nil
		default:
			return "", "credential_pending", nil
		}
	}
	x.key, _ = x.r.master.Key(x.tenant, *c.CredentialID)
	x.r.redact(x.key)
	if c.Definition.SchemaVersion == 2 || c.Mode == "preview" {
		return x.stepsV2(ctx)
	}
	env := Env{Inputs: c.Inputs, Outputs: map[string]any{}}
	for i, st := range c.Definition.Steps {
		active, err := x.beat(ctx)
		if err != nil {
			return "", "", err
		}
		if !active {
			return "", "version_replaced", nil
		}
		if st.Kind == "respond" {
			text, err := renderText(st.Text, env)
			switch {
			case err != nil:
				return "", "result_unavailable", nil
			case len(text) > maxAnswer:
				return "", "answer_too_large", nil
			}
			return text, "", nil
		}
		var id uuid.UUID
		if i < len(c.Steps) {
			id = c.Steps[i].ActionID // recorded: waited on, never resent
		} else {
			if id, reason, err = x.submit(ctx, i, st, env); err != nil || reason != "" {
				return "", reason, err
			}
			if err := x.record(ctx, i, id); err != nil {
				return "", "", err
			}
		}
		out, reason, err := x.await(ctx, id, x.needed(i))
		if err != nil || reason != "" {
			return "", reason, err
		}
		if out != nil {
			env.Outputs[st.ID] = out
		}
	}
	return "", "", errors.New("studioruntime: a definition without a respond step")
}

// needed reports whether a step after i refers to step i's output.
func (x *execution) needed(i int) bool {
	steps := x.c.Definition.Steps
	ref := "{{steps." + steps[i].ID + ".output"
	for _, s := range steps[i+1:] {
		if strings.Contains(string(s.Payload), ref) || strings.Contains(s.Text, ref) || strings.Contains(string(s.Input), ref) || strings.Contains(s.Instruction, ref) || strings.Contains(string(s.Condition.Left), ref) || strings.Contains(string(s.Condition.Right), ref) {
			return true
		}
	}
	return false
}

// submit sends step i as the run's agent under the step's idempotency key,
// so a resubmission after a crash returns the same action.
func (x *execution) submit(ctx context.Context, i int, st step, env Env) (uuid.UUID, string, error) {
	raw, err := decodeJSON(st.Payload)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("studioruntime: step %d payload: %w", i, err)
	}
	payload, err := Render(raw, env)
	if err != nil {
		return uuid.Nil, "result_unavailable", nil
	}
	body := map[string]any{"subject": x.c.Subject, "operation": st.Operation, "target": st.Target,
		"tool": st.Tool, "tool_schema_version": st.ToolSchemaVersion, "resource": st.Resource, "payload": payload}
	header := map[string]string{"Idempotency-Key": "studio:" + x.c.ID.String() + ":" + strconv.Itoa(i)}
	for {
		var view struct {
			ID uuid.UUID `json:"id"`
		}
		status, err := x.r.api.do(ctx, x.key, "POST", "/v1/actions", header, body, &view)
		switch {
		case err == nil:
			return view.ID, "", nil
		case status == 401:
			reason, err := x.unauthorized(ctx)
			return uuid.Nil, reason, err
		case status >= 400 && status < 500 && status != 429:
			return uuid.Nil, "action_failed", nil
		}
		if err := x.pause(ctx, err); err != nil {
			return uuid.Nil, "", err
		}
	}
}

// record records step i's action; PostgreSQL checks it is the run's own.
func (x *execution) record(ctx context.Context, i int, id uuid.UUID) error {
	for {
		status, err := x.r.api.do(ctx, x.r.keys[x.tenant], "POST", "/v1/studio/runtime/runs/"+x.c.ID.String()+"/steps",
			nil, map[string]any{"runtime_id": x.r.id, "generation": x.c.Generation, "index": i, "action_id": id}, nil)
		switch {
		case err == nil:
			return nil
		case status == 403:
			return errLost
		case status >= 400 && status < 500:
			return err
		}
		if err := x.pause(ctx, err); err != nil {
			return err
		}
	}
}

// await waits for action id to settle and returns its output when want.
func (x *execution) await(ctx context.Context, id uuid.UUID, want bool) (any, string, error) {
	for {
		if x.killed.Load() {
			return nil, "", errKilled
		}
		if x.replaced.Load() {
			return nil, "version_replaced", nil
		}
		wait := maxWaitSeconds
		if d, ok := ctx.Deadline(); ok {
			wait = max(min(wait, int(time.Until(d)/time.Second)), 1)
		}
		var view struct {
			State       string `json:"state"`
			StateReason string `json:"state_reason"`
		}
		status, err := x.r.api.do(ctx, x.key, "GET", "/v1/actions/"+id.String()+"?wait="+strconv.Itoa(wait)+"s",
			nil, nil, &view)
		switch {
		case status == 401:
			reason, err := x.unauthorized(ctx)
			return nil, reason, err
		case err != nil:
			if err := x.pause(ctx, err); err != nil {
				return nil, "", err
			}
			continue
		}
		switch view.State {
		case "SUCCEEDED":
			if !want {
				return nil, "", nil
			}
			return x.result(ctx, id)
		case "DENIED":
			if view.StateReason == "agent_version_not_active" {
				return nil, "version_replaced", nil
			}
			return nil, "action_denied", nil
		case "FAILED", "EXPIRED":
			return nil, "action_failed", nil
		case "CANCELLED":
			return nil, "action_cancelled", nil
		case "UNKNOWN_OUTCOME", "NEEDS_HUMAN_RESOLUTION":
			return nil, "action_unknown", nil
		}
	}
}

// result reads a success's kept output through the result channel.
func (x *execution) result(ctx context.Context, id uuid.UUID) (any, string, error) {
	for {
		var res struct {
			Output json.RawMessage `json:"output"`
		}
		status, err := x.r.api.do(ctx, x.key, "GET", "/v1/actions/"+id.String()+"/result", nil, nil, &res)
		switch {
		case err == nil:
			out, err := decodeJSON(res.Output)
			if err != nil {
				return nil, "result_unavailable", nil
			}
			return out, "", nil
		case status == 401:
			reason, err := x.unauthorized(ctx)
			return nil, reason, err
		case status >= 400 && status < 500:
			return nil, "result_unavailable", nil
		}
		if err := x.pause(ctx, err); err != nil {
			return nil, "", err
		}
	}
}

// unauthorized explains a refused agent key: the version was replaced, or
// the key was revoked or expired.
func (x *execution) unauthorized(ctx context.Context) (string, error) {
	active, err := x.beat(ctx)
	switch {
	case err != nil:
		return "", err
	case !active:
		return "version_replaced", nil
	}
	return "credential_revoked", nil
}

// pause waits before a retry of a transient failure, or returns ctx's end.
func (x *execution) pause(ctx context.Context, cause error) error {
	d := time.Second
	var ae *apiError
	if errors.As(cause, &ae) && ae.RetryAfter > 0 {
		d = min(ae.RetryAfter, 5*time.Second)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
