// Package llmgateway is the LLM gateway (ADR-031): an ingress that lets an
// agent call Anthropic Messages or OpenAI Chat Completions through EACP. It
// authenticates the agent, asks the PDP with no transaction open, admits the
// call in PostgreSQL (capability, kill, budget), forwards it with a provider
// key only the gateway holds, relays the answer, and settles the usage in
// PostgreSQL, which prices it. It never stores, journals or logs a prompt, a
// response or a key.
package llmgateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"eacp/internal/governance"
	"eacp/internal/identity"
	"eacp/internal/llm"
	"eacp/internal/worker"
)

// Bounds (ADR-031).
const (
	DefaultMaxRequestBytes = 4 << 20
	MaxRequestBytesLimit   = 32 << 20
	maxResponseBytes       = 16 << 20
	maxEventBytes          = 1 << 20
	pdpTimeout             = 5 * time.Second
	settleTimeout          = 15 * time.Second
	defaultKillPoll        = 2 * time.Second
)

// Authenticator resolves an API key to its caller (identity.Authenticate).
type Authenticator func(ctx context.Context, key string) (identity.Caller, error)

// Ledger is the LLM-call ledger (llm.Store).
type Ledger interface {
	Admit(ctx context.Context, tenant uuid.UUID, r llm.AdmitRequest) (llm.Admission, error)
	Settle(ctx context.Context, tenant, call uuid.UUID, s llm.Settlement) error
	Killed(ctx context.Context, tenant, call uuid.UUID) (bool, error)
	KillEpoch(ctx context.Context, tenant uuid.UUID) (int64, error)
}

// Policies returns a tenant's current policy (governance.Store).
type Policies interface {
	CurrentPolicy(ctx context.Context, tenant uuid.UUID) (governance.Policy, error)
}

// Options configure a Gateway.
type Options struct {
	ID              string // this replica, recorded on every call
	Auth            Authenticator
	Ledger          Ledger
	Policies        Policies
	PDP             governance.GovernanceProvider
	Secrets         *worker.SecretStore
	AgentRisk       func(ctx context.Context, tenant, agent uuid.UUID) (string, error)
	MaxRequestBytes int64         // default 4 MiB, at most 32 MiB
	KillPoll        time.Duration // default 2 s
	Log             *slog.Logger
	// HTTP overrides the upstream client (tests). The gateway never follows
	// a redirect, whatever the client says.
	HTTP *http.Client
}

// Gateway serves the two provider routes.
type Gateway struct {
	o      Options
	client *http.Client
}

var gatewayID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// New checks o and returns a Gateway.
func New(o Options) (*Gateway, error) {
	switch {
	case !gatewayID.MatchString(o.ID):
		return nil, errors.New("llmgateway: the gateway id must be 1-128 characters of letters, digits and ._:-")
	case o.Auth == nil || o.Ledger == nil || o.Policies == nil || o.PDP == nil || o.Secrets == nil || o.AgentRisk == nil:
		return nil, errors.New("llmgateway: authentication, ledger, policies, PDP, secrets and agent risk are required")
	case o.MaxRequestBytes < 0 || o.MaxRequestBytes > MaxRequestBytesLimit:
		return nil, fmt.Errorf("llmgateway: the request bound must be at most %d bytes", MaxRequestBytesLimit)
	case o.KillPoll < 0:
		return nil, errors.New("llmgateway: the kill poll must be positive")
	}
	if o.MaxRequestBytes == 0 {
		o.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if o.KillPoll == 0 {
		o.KillPoll = defaultKillPoll
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	var client http.Client
	if o.HTTP != nil {
		client = *o.HTTP
	} else {
		// No proxy: provider traffic goes only where the model says.
		client.Transport = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Gateway{o: o, client: &client}, nil
}

// ServeHTTP serves POST /v1/messages and POST /v1/chat/completions.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == anthropic.route():
		g.serve(w, r, anthropic)
	case r.Method == http.MethodPost && r.URL.Path == openai.route():
		g.serve(w, r, openai)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}
}

// call is one request as it moves through the gateway.
type call struct {
	api    api
	caller identity.Caller
	req    request
	id     uuid.UUID
	model  llm.Model
	log    *slog.Logger
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, a api) {
	ctx := r.Context()
	key, ok := agentKey(r, a)
	if !ok {
		g.fail(w, a, http.StatusUnauthorized, "unauthenticated", "")
		return
	}
	caller, err := g.o.Auth(ctx, key)
	if err != nil || caller.Kind != identity.KindAgent {
		reason := "not_an_agent_key"
		var ae *identity.AuthError
		if errors.As(err, &ae) {
			reason = ae.Reason
		} else if err != nil {
			reason = "internal"
		}
		g.o.Log.WarnContext(ctx, "llm authentication failed", "reason", reason, "route", a.route())
		g.fail(w, a, http.StatusUnauthorized, "unauthenticated", "")
		return
	}
	log := g.o.Log.With("tenant", caller.TenantID.String(), "agent", caller.AgentID.String(),
		"agent_version", caller.AgentVersionID.String())
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.o.MaxRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			g.fail(w, a, http.StatusRequestEntityTooLarge, "request_too_large", "")
			return
		}
		g.fail(w, a, http.StatusBadRequest, "invalid_request", "the body could not be read")
		return
	}
	req, err := parse(a, body)
	if err != nil {
		var bad invalid
		errors.As(err, &bad)
		g.fail(w, a, http.StatusBadRequest, "invalid_request", bad.why)
		return
	}
	subject := r.Header.Get("EACP-Subject")
	if len(subject) > 256 || hasControl(subject) {
		g.fail(w, a, http.StatusBadRequest, "invalid_request", "EACP-Subject must be at most 256 characters")
		return
	}
	c := &call{api: a, caller: caller, req: req, log: log.With("model", req.model)}
	tenant := caller.TenantID

	// The kill epoch before admission: any kill after it is seen below.
	epoch, err := g.o.Ledger.KillEpoch(ctx, tenant)
	if err != nil {
		c.log.ErrorContext(ctx, "llm kill epoch unavailable", "err", err)
		g.unavailable(w, a, "ledger_unavailable")
		return
	}
	decision, ok := g.decide(ctx, w, c, subject, len(body))
	if !ok {
		return
	}
	adm, err := g.o.Ledger.Admit(ctx, tenant, llm.AdmitRequest{AgentVersionID: caller.AgentVersionID,
		ModelName: req.model, Provider: a.provider(), Subject: subject, TraceID: traceID(r.Header.Get("traceparent")),
		GatewayID: g.o.ID, Stream: req.stream, RequestBytes: int64(len(body)), MaxOutputTokens: req.maxOut,
		Decision: decision})
	if err != nil {
		c.log.ErrorContext(ctx, "llm admission failed", "err", err)
		g.unavailable(w, a, "ledger_unavailable")
		return
	}
	c.id, c.model = adm.CallID, adm.Model
	c.log = c.log.With("call", c.id.String())
	if adm.Denial != "" {
		c.log.InfoContext(ctx, "llm call denied", "denial", adm.Denial)
		g.fail(w, a, http.StatusForbidden, adm.Denial, "")
		return
	}
	g.forward(w, r, c, epoch)
}

// decide asks the PDP with no transaction open (ADR-005 §5a) and maps its
// verdict to a denial code the admission applies in order.
func (g *Gateway) decide(ctx context.Context, w http.ResponseWriter, c *call, subject string,
	size int) (llm.Decision, bool) {
	tenant := c.caller.TenantID
	pol, err := g.o.Policies.CurrentPolicy(ctx, tenant)
	if err != nil {
		c.log.WarnContext(ctx, "no usable policy; LLM call not evaluated", "err", err)
		g.unavailable(w, c.api, "governance_unavailable")
		return llm.Decision{}, false
	}
	risk, err := g.o.AgentRisk(ctx, tenant, c.caller.AgentID)
	if err != nil {
		c.log.ErrorContext(ctx, "agent risk unavailable", "err", err)
		g.unavailable(w, c.api, "ledger_unavailable")
		return llm.Decision{}, false
	}
	if subject == "" {
		subject = "agent:" + c.caller.AgentID.String()
	}
	maxOut := "null"
	if c.req.maxOut > 0 {
		maxOut = strconv.FormatInt(c.req.maxOut, 10)
	}
	payload := fmt.Appendf(nil, `{"max_output_tokens":%s,"model":%s,"request_bytes":%d,"stream":%t}`,
		maxOut, jsonString(c.req.model), size, c.req.stream)
	pctx, cancel := context.WithTimeout(ctx, pdpTimeout)
	defer cancel()
	d, err := governance.EvaluateChecked(pctx, g.o.PDP, governance.GovernanceRequest{
		Binding: governance.Binding{TenantID: tenant, AgentID: c.caller.AgentID, AgentVersionID: c.caller.AgentVersionID,
			Subject: subject, Operation: "llm.generate", Target: c.req.model, Tool: "llm:" + c.api.provider(),
			ToolSchemaVersion: "1", Resource: c.req.model, Payload: payload},
		RiskClass: risk, SideEffectClass: "LLM_GENERATION", PolicyBundleID: pol.ID, PolicyVersion: pol.Version,
		Policy: pol.Content,
	})
	if err != nil {
		if errors.Is(err, governance.ErrMalformedDecision) {
			c.log.ErrorContext(ctx, "security alert", "alert", "governance.malformed_decision", "err", err)
		} else {
			c.log.WarnContext(ctx, "governance unavailable", "err", err)
		}
		g.unavailable(w, c.api, "governance_unavailable")
		return llm.Decision{}, false
	}
	if d.DigestMismatch {
		c.log.ErrorContext(ctx, "security alert", "alert", "governance.digest_mismatch")
	}
	out := llm.Decision{ID: d.DecisionID, BundleID: d.PolicyBundleID, Version: d.PolicyVersion,
		Verdict: string(d.Verdict), InputDigest: d.InputDigest}
	switch d.Verdict {
	case governance.VerdictDeny:
		out.Denial = "policy_denied"
		if len(d.Reasons) > 0 && denialCode.MatchString(d.Reasons[0]) {
			out.Denial = d.Reasons[0]
		}
	case governance.VerdictEscalate:
		out.Denial = "approval_unsupported" // no synchronous approval for an LLM call
	case governance.VerdictTransform:
		out.Denial = "transform_unsupported" // the gateway never rewrites a call
	default:
		if d.EnforcedDigest != d.InputDigest {
			out.Denial = "transform_unsupported"
		}
	}
	return out, true
}

var denialCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// forward sends an admitted call upstream, relays the answer and settles it.
// It sends at most one request and never retries.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, c *call, epoch int64) {
	ctx := r.Context()
	tenant := c.caller.TenantID
	// A kill between the epoch read and the admission stops the call here.
	if now, err := g.o.Ledger.KillEpoch(ctx, tenant); err != nil || now != epoch {
		killed, kerr := g.o.Ledger.Killed(ctx, tenant, c.id)
		if err != nil || kerr != nil {
			c.log.ErrorContext(ctx, "llm kill check failed", "err", errors.Join(err, kerr))
			g.settle(ctx, c, llm.OutcomeProviderError, 0, llm.Usage{})
			g.unavailable(w, c.api, "ledger_unavailable")
			return
		}
		if killed {
			g.settle(ctx, c, llm.OutcomeKilled, 0, llm.Usage{})
			g.fail(w, c.api, http.StatusForbidden, "killed", "")
			return
		}
		epoch = now
	}
	endpoint := strings.TrimRight(c.model.BaseURL, "/") + c.api.route()
	secret, err := g.o.Secrets.Credential(ctx, tenant, c.model.SecretRef, endpoint, c.model.Timeout+worker.CredentialSkew)
	if err != nil || secret.SignsRequests() {
		c.log.ErrorContext(ctx, "no provider credential", "secret_ref", c.model.SecretRef, "err", err)
		g.settle(ctx, c, llm.OutcomeProviderError, 0, llm.Usage{})
		g.unavailable(w, c.api, "credential_unavailable")
		return
	}

	upCtx, cancel := context.WithTimeout(ctx, c.model.Timeout)
	defer cancel()
	var killed atomic.Bool
	stop := g.watch(upCtx, cancel, c, epoch, &killed)
	defer stop()

	body := rewrite(c.api, c.req, c.model)
	up, err := http.NewRequestWithContext(upCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		g.settle(ctx, c, llm.OutcomeProviderError, 0, llm.Usage{})
		g.fail(w, c.api, http.StatusBadGateway, "provider_unreachable", "")
		return
	}
	up.Header.Set("Content-Type", "application/json")
	for _, h := range append([]string{"Accept", "traceparent", "tracestate"}, c.api.forwarded()...) {
		if v := r.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	c.api.authorize(up.Header, secret)
	resp, err := g.client.Do(up)
	if err != nil {
		g.lost(w, r, c, upCtx, &killed, 0, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		g.o.Secrets.Rejected(tenant, c.model.SecretRef, secret)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		g.relayError(w, r, c, upCtx, &killed, resp)
		return
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/event-stream" {
		g.relayStream(w, r, c, upCtx, &killed, resp)
		return
	}
	data, tooLarge, err := readLimited(resp.Body, maxResponseBytes)
	switch {
	case err != nil:
		g.lost(w, r, c, upCtx, &killed, resp.StatusCode, err)
		return
	case tooLarge:
		g.settle(ctx, c, llm.OutcomeUsageUnknown, resp.StatusCode, llm.Usage{})
		g.fail(w, c.api, http.StatusBadGateway, "response_too_large", "")
		return
	}
	usage, ok := c.api.usage(data)
	relayHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
	if ok {
		g.settle(ctx, c, llm.OutcomeSucceeded, resp.StatusCode, usage)
	} else {
		g.settle(ctx, c, llm.OutcomeUsageUnknown, resp.StatusCode, llm.Usage{})
	}
}

// watch polls the tenant's kill epoch every KillPoll while the call runs;
// when it moves and a scope now stops the call, it cancels the upstream
// request. The returned stop waits for the watcher to end.
func (g *Gateway) watch(ctx context.Context, cancel context.CancelFunc, c *call, epoch int64,
	killed *atomic.Bool) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(g.o.KillPoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			now, err := g.o.Ledger.KillEpoch(ctx, c.caller.TenantID)
			if err != nil {
				c.log.WarnContext(ctx, "llm kill poll failed", "err", err)
				continue
			}
			if now == epoch {
				continue
			}
			k, err := g.o.Ledger.Killed(ctx, c.caller.TenantID, c.id)
			if err != nil {
				c.log.WarnContext(ctx, "llm kill check failed", "err", err)
				continue
			}
			epoch = now
			if k {
				killed.Store(true)
				cancel()
				return
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// lost settles a call whose upstream request or response failed: killed,
// the client gone, the model's timeout, or the provider unreachable. The
// provider may have done the work, so its usage is unknown.
func (g *Gateway) lost(w http.ResponseWriter, r *http.Request, c *call, upCtx context.Context,
	killed *atomic.Bool, status int, err error) {
	ctx := r.Context()
	switch {
	case killed.Load():
		g.settle(ctx, c, llm.OutcomeKilled, status, llm.Usage{})
		g.fail(w, c.api, http.StatusForbidden, "killed", "")
	case ctx.Err() != nil:
		g.settle(ctx, c, llm.OutcomeUsageUnknown, status, llm.Usage{})
	case errors.Is(upCtx.Err(), context.DeadlineExceeded):
		g.settle(ctx, c, llm.OutcomeUsageUnknown, status, llm.Usage{})
		g.fail(w, c.api, http.StatusGatewayTimeout, "provider_timeout", "")
	default:
		c.log.WarnContext(ctx, "llm provider unreachable", "err", err)
		g.settle(ctx, c, llm.OutcomeUsageUnknown, status, llm.Usage{})
		g.fail(w, c.api, http.StatusBadGateway, "provider_unreachable", "")
	}
}

// relayError relays a provider's non-2xx answer: nothing was generated, so
// the reservation is released.
func (g *Gateway) relayError(w http.ResponseWriter, r *http.Request, c *call, upCtx context.Context,
	killed *atomic.Bool, resp *http.Response) {
	data, tooLarge, err := readLimited(resp.Body, maxResponseBytes)
	if err != nil {
		g.lost(w, r, c, upCtx, killed, resp.StatusCode, err)
		return
	}
	g.settle(r.Context(), c, llm.OutcomeProviderError, resp.StatusCode, llm.Usage{})
	if tooLarge {
		g.fail(w, c.api, http.StatusBadGateway, "response_too_large", "")
		return
	}
	relayHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

// relayStream relays a stream event by event, flushing each, and reads the
// usage from the events without keeping them.
func (g *Gateway) relayStream(w http.ResponseWriter, r *http.Request, c *call, upCtx context.Context,
	killed *atomic.Bool, resp *http.Response) {
	ctx := r.Context()
	relayHeaders(w, resp)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	t := c.api.tracker()
	events := newEventReader(resp.Body, maxEventBytes)
	var end error
	for {
		ev, err := events.next()
		if err != nil {
			end = err
			break
		}
		if len(ev.data) > 0 {
			t.observe(ev.data)
		}
		if _, err := w.Write(ev.raw); err != nil {
			end = err
			break
		}
		if err := rc.Flush(); err != nil {
			end = err
			break
		}
	}
	switch {
	case killed.Load():
		_, _ = w.Write(c.api.streamError(http.StatusForbidden, "killed"))
		_ = rc.Flush()
		g.settle(ctx, c, llm.OutcomeKilled, resp.StatusCode, llm.Usage{})
	case errors.Is(end, io.EOF):
		if u, ok := t.usage(); ok {
			g.settle(ctx, c, llm.OutcomeSucceeded, resp.StatusCode, u)
		} else {
			g.settle(ctx, c, llm.OutcomeUsageUnknown, resp.StatusCode, llm.Usage{})
		}
	default:
		switch {
		case errors.Is(end, errEventTooLarge):
			_, _ = w.Write(c.api.streamError(http.StatusBadGateway, "event_too_large"))
		case ctx.Err() == nil && errors.Is(upCtx.Err(), context.DeadlineExceeded):
			_, _ = w.Write(c.api.streamError(http.StatusGatewayTimeout, "provider_timeout"))
		case ctx.Err() == nil && !errors.Is(end, io.ErrUnexpectedEOF):
			c.log.WarnContext(ctx, "llm stream ended early", "err", end)
		}
		_ = rc.Flush()
		g.settle(ctx, c, llm.OutcomeUsageUnknown, resp.StatusCode, llm.Usage{})
	}
}

// settle records how a call ended; a settlement that fails is left to the
// sweeper, which abandons the call at its deadline.
func (g *Gateway) settle(ctx context.Context, c *call, outcome string, status int, u llm.Usage) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	err := g.o.Ledger.Settle(sctx, c.caller.TenantID, c.id, llm.Settlement{Outcome: outcome, ProviderStatus: status, Usage: u})
	attrs := []any{"outcome", outcome, "status", status, "stream", c.req.stream, "input_tokens", u.Input,
		"cache_read_tokens", u.CacheRead, "cache_write_tokens", u.CacheWrite, "output_tokens", u.Output}
	if err != nil {
		c.log.ErrorContext(ctx, "llm settlement failed", append(attrs, "err", err)...)
		return
	}
	c.log.InfoContext(ctx, "llm call settled", attrs...)
}

// fail writes a provider-shaped error.
func (g *Gateway) fail(w http.ResponseWriter, a api, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(a.errorBody(status, code, detail))
}

// unavailable is a retryable 503.
func (g *Gateway) unavailable(w http.ResponseWriter, a api, code string) {
	w.Header().Set("Retry-After", "1")
	g.fail(w, a, http.StatusServiceUnavailable, code, "")
}

// agentKey reads the agent's EACP key: x-api-key (Anthropic) or a Bearer.
// Two different keys are refused.
func agentKey(r *http.Request, a api) (string, bool) {
	bearer, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	bearer = strings.TrimSpace(bearer)
	if a == anthropic {
		if x := strings.TrimSpace(r.Header.Get("x-api-key")); x != "" {
			if hasBearer && bearer != x {
				return "", false
			}
			return x, true
		}
	}
	return bearer, hasBearer && bearer != ""
}

// relayHeaders copies the provider headers an agent may see.
func relayHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, h := range []string{"Content-Type", "request-id", "x-request-id"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
}

// readLimited reads at most limit bytes; tooLarge reports more.
func readLimited(r io.Reader, limit int) (data []byte, tooLarge bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > limit {
		return nil, true, nil
	}
	return data, false, nil
}

var traceparent = regexp.MustCompile(`^[0-9a-f]{2}-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)

// traceID is the W3C trace id of a traceparent header, or "".
func traceID(h string) string {
	m := traceparent.FindStringSubmatch(strings.TrimSpace(h))
	if m == nil || m[1] == strings.Repeat("0", 32) {
		return ""
	}
	if _, err := hex.DecodeString(m[1]); err != nil {
		return ""
	}
	return m[1]
}
