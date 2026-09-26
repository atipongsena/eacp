package worker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/action"
	"eacp/internal/connector"
	"eacp/internal/fakeerp"
	"eacp/internal/governance"
	"eacp/internal/jwttest"
	"eacp/internal/logging"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
	"eacp/internal/worker"
)

// jitEnv is a tenant whose ERP connector authenticates with tokens minted by
// the Fake ERP's token endpoint (ADR-019), behind a switch that makes the
// endpoint fail.
type jitEnv struct {
	t       *testing.T
	f       *registrytest.Fixture
	srv     *httptest.Server
	engine  *action.Engine
	agent   registrytest.Agent
	secrets *worker.SecretStore
	w       *worker.Worker
	logs    *bytes.Buffer
	logMu   *sync.Mutex
	failing atomic.Bool
	refuse  atomic.Bool // answer /v1/execute with 401 unauthorized
	mints   atomic.Int64
	mu      sync.Mutex
	tokens  []string // every token the endpoint issued
	clock   *clock
	logger  *slog.Logger
}

const jitStatic = canary + "-jit-static"

func newJIT(t *testing.T, ttl time.Duration, timeoutMS int) *jitEnv {
	t.Helper()
	return newJITWith(t, fakeerp.Options{OAuthClientID: "eacp-worker", OAuthClientSecret: canary + "-client", TokenTTL: ttl},
		fmt.Sprintf(`"client_id":"eacp-worker","client_secret":%q,"scope":"erp.purchase"`, canary+"-client"), timeoutMS)
}

// newJITWith is newJIT with the Fake ERP's OAuth options and the worker's
// oauth2 client fields (everything but token_url) chosen by the test.
func newJITWith(t *testing.T, erp fakeerp.Options, client string, timeoutMS int) *jitEnv {
	t.Helper()
	return newJITFor(t, func(string) fakeerp.Options { return erp }, func(string) string { return client }, timeoutMS)
}

// newJITFor is newJITWith for options that name the token endpoint's URL
// (a private_key_jwt audience): both are built from it.
func newJITFor(t *testing.T, erpFor func(tokenURL string) fakeerp.Options, clientFor func(tokenURL string) string,
	timeoutMS int) *jitEnv {
	t.Helper()
	v := &jitEnv{t: t, clock: &clock{t: time.Now()}, logs: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	var handler atomic.Value // the Fake ERP, built once the server's URL is known
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(http.Handler).ServeHTTP(w, r) })
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/execute" && v.refuse.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error_class":"unauthorized"}`))
			return
		}
		if r.URL.Path != "/oauth/token" {
			h.ServeHTTP(w, r)
			return
		}
		v.mints.Add(1)
		if v.failing.Load() {
			http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var got struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		v.mu.Lock()
		v.tokens = append(v.tokens, got.AccessToken)
		v.mu.Unlock()
		for k, vals := range rec.Header() {
			w.Header()[k] = vals
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(v.srv.Close)
	tokenURL := v.srv.URL + "/oauth/token"
	erp, err := fakeerp.NewWithOptions(jitStatic, filepath.Join(t.TempDir(), "erp.log"), erpFor(tokenURL))
	if err != nil {
		t.Fatal(err)
	}
	handler.Store(erp)
	u, _ := url.Parse(v.srv.URL)
	v.f = registrytest.New(t)
	connID := v.f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'erp', 'http', $1, 'erp-jit') RETURNING id`, v.srv.URL)
	toolID := v.f.ID(t, "erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, 'create_po') RETURNING id`, connID)
	contract := strings.Replace(httpContractSQL, "2, 100)", fmt.Sprintf("2, %d)", timeoutMS), 1)
	contractID := v.f.ID(t, "erin", contract, toolID)
	if err := v.f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, contractID, toolID); err != nil {
		t.Fatal(err)
	}
	// A second tool on the same connector (and credential binding) whose calls
	// may take 400 s: longer than any token the ERP issues in these tests.
	longID := v.f.ID(t, "erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, 'create_po_eventual') RETURNING id`, connID)
	longContract := v.f.ID(t, "erin", strings.Replace(httpContractSQL, "2, 100)", "2, 300000)", 1), longID)
	if err := v.f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, longContract, longID); err != nil {
		t.Fatal(err)
	}
	v.agent = v.f.ActiveAgent(t, "buyer", toolID, longID)
	v.f.ActivatePolicy(t, registrytest.AllowPolicy)
	set := logging.NewSecretSet()
	logger := logging.NewWithSet(syncWriter{v.logs, v.logMu}, slog.LevelDebug, "json", set)
	v.logger = logger
	v.secrets, err = worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp-jit","host":%q,
		"oauth2":{"token_url":%q,%s}}]}`, pgtest.TenantA, u.Host, tokenURL, clientFor(tokenURL))),
		worker.AllowPlainTokenURL(), worker.WithRedaction(set), worker.WithClock(v.clock.now), worker.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	v.w, err = worker.New(v.f.App, worker.Options{ID: "jit", Lease: 5 * time.Second, Log: logger,
		Connectors: map[string]worker.Connector{"http": connector.NewHTTP()}, Secrets: v.secrets,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	v.engine = action.New(v.f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "jit-test"}})
	return v
}

func (v *jitEnv) submit(scenario string) action.View {
	v.t.Helper()
	return v.submitTool("erp.create_po", scenario)
}

func (v *jitEnv) submitTool(tool, scenario string) action.View {
	v.t.Helper()
	payload := map[string]any{"amount": 7, "currency": "THB"}
	if scenario != "" {
		payload["scenario"], payload["delay_ms"] = scenario, 300
	}
	b, _ := json.Marshal(payload)
	view, err := v.engine.Submit(context.Background(), action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version),
		action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "post",
			Target: "erp", Tool: tool, ToolSchemaVersion: "1", Resource: "po", Payload: b})
	if err != nil || view.State != "QUEUED" {
		v.t.Fatalf("submit = %+v, %v", view, err)
	}
	return view
}

func (v *jitEnv) get(id uuid.UUID) action.View {
	v.t.Helper()
	got, err := v.engine.Get(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return got
}

func (v *jitEnv) run(want int) {
	v.t.Helper()
	if n, err := v.w.RunOnce(context.Background()); err != nil || n != want {
		v.t.Fatalf("worker claimed %d, want %d (err %v)", n, want, err)
	}
}

// audit returns the Fake ERP audit, read with the static credential.
func (v *jitEnv) audit() []map[string]any {
	v.t.Helper()
	req, _ := http.NewRequest("GET", v.srv.URL+"/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+jitStatic)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		v.t.Fatal(err)
	}
	return out
}

func (v *jitEnv) log() string {
	v.logMu.Lock()
	defer v.logMu.Unlock()
	return v.logs.String()
}

func TestTheWorkerExecutesWithAMintedToken(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	ctx := context.Background()
	var ids []uuid.UUID
	for range 3 {
		ids = append(ids, v.submit("").ID)
	}
	for claimed, tries := 0, 0; claimed < 3; tries++ {
		n, err := v.w.RunOnce(ctx)
		if err != nil || tries == 5 {
			t.Fatalf("claimed %d of 3 (err %v)", claimed, err)
		}
		claimed += n
	}
	for _, id := range ids {
		if got := v.get(id); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
			t.Fatalf("action = %+v", got)
		}
	}
	if v.mints.Load() != 1 {
		t.Fatalf("%d mints for 3 calls, want one token reused", v.mints.Load())
	}
	executes := 0
	for _, e := range v.audit() {
		if e["path"] == "/v1/execute" {
			executes++
			if e["principal"] != "oauth:eacp-worker" {
				t.Fatalf("an execute used principal %v, not the minted token", e["principal"])
			}
		}
	}
	if executes != 3 {
		t.Fatalf("%d executes, want 3", executes)
	}
	v.mu.Lock()
	values := append([]string{canary + "-client"}, v.tokens...)
	v.mu.Unlock()
	if len(values) != 2 {
		t.Fatalf("issued %d tokens", len(values)-1)
	}
	v.assertNotPersisted(values...)
	if !strings.Contains(v.log(), "credential minted") {
		t.Fatal("the mint was not logged")
	}
}

// assertNotPersisted fails unless no value appears in the tenant's actions,
// attempts, audit events, outbox or the worker log.
func (v *jitEnv) assertNotPersisted(values ...string) {
	v.t.Helper()
	ctx := context.Background()
	for _, value := range values {
		for _, sql := range []string{
			`SELECT count(*) FROM eacp.actions WHERE to_jsonb(actions)::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.action_attempts WHERE to_jsonb(action_attempts)::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.outbox_events WHERE to_jsonb(outbox_events)::text LIKE '%' || $1 || '%'`,
		} {
			var count int
			if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, sql, value).Scan(&count)
			}); err != nil || count != 0 {
				v.t.Fatalf("a credential was persisted (count=%d err=%v)", count, err)
			}
		}
		if strings.Contains(v.log(), value) {
			v.t.Fatal("a credential reached the worker log")
		}
	}
}

// TestTheWorkerExecutesThroughWorkloadIdentityFederation: the binding holds no
// client secret; the worker presents its platform-issued assertion and the
// ERP (a federated relying party) mints the token (ADR-019 Rev 1.1).
func TestTheWorkerExecutesThroughWorkloadIdentityFederation(t *testing.T) {
	signer := jwttest.New(t)
	keys, err := fakeerp.ParseJWKS(signer.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	const subject = "system:serviceaccount:eacp:eacp-worker"
	now := time.Now()
	jwt := signer.Sign(map[string]any{"iss": "https://issuer.test", "sub": subject, "aud": []string{"fakeerp"},
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix()})
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(jwt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := newJITWith(t, fakeerp.Options{TokenTTL: 10 * time.Minute, Federated: &fakeerp.Federated{ClientID: "eacp-wif",
		Issuer: "https://issuer.test", Audience: "fakeerp", Subject: subject, Keys: keys}},
		fmt.Sprintf(`"client_id":"eacp-wif","client_assertion_file":%q`, file), 100)
	ids := []uuid.UUID{v.submit("").ID, v.submit("").ID}
	for claimed, tries := 0, 0; claimed < 2; tries++ {
		n, err := v.w.RunOnce(context.Background())
		if err != nil || tries == 5 {
			t.Fatalf("claimed %d of 2 (err %v)", claimed, err)
		}
		claimed += n
	}
	for _, id := range ids {
		if got := v.get(id); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
			t.Fatalf("action = %+v", got)
		}
	}
	executes := 0
	for _, e := range v.audit() {
		if e["path"] == "/v1/execute" {
			executes++
			if e["principal"] != "oauth:eacp-wif" {
				t.Fatalf("an execute used principal %v, not a federated token", e["principal"])
			}
		}
	}
	if executes != 2 {
		t.Fatalf("%d executes, want 2", executes)
	}
	v.mu.Lock()
	values := append([]string{jwt}, v.tokens...)
	v.mu.Unlock()
	if len(values) != 2 {
		t.Fatalf("issued %d tokens, want one reused", len(values)-1)
	}
	v.assertNotPersisted(values...)
}

// TestTheWorkerExecutesWithPrivateKeyJWT: the binding holds the worker's own
// signing key; each mint presents a fresh assertion signed with it and the ERP
// verifies it against the client's registered certificate (ADR-019 Rev 1.2).
func TestTheWorkerExecutesWithPrivateKeyJWT(t *testing.T) {
	key := rsaKey(t, 2048)
	keyPEM := pkcs8PEM(t, key)
	now := time.Now()
	cert, certPEM := selfSigned(t, key, now.Add(-time.Hour), now.Add(24*time.Hour))
	sum := sha256.Sum256(cert.Raw)
	thumb := base64.RawURLEncoding.EncodeToString(sum[:])
	v := newJITFor(t, func(tokenURL string) fakeerp.Options {
		return fakeerp.Options{TokenTTL: 10 * time.Minute, KeyClient: &fakeerp.KeyClient{ClientID: "eacp-pkjwt",
			Audience: tokenURL, Keys: []fakeerp.PublicKey{{X5TS256: thumb, Key: &key.PublicKey}}}}
	}, func(string) string {
		k, _ := json.Marshal(keyPEM)
		c, _ := json.Marshal(certPEM)
		return fmt.Sprintf(`"client_id":"eacp-pkjwt","private_key_jwt":{"alg":"PS256","key":%s,"certificate":%s}`, k, c)
	}, 100)
	ids := []uuid.UUID{v.submit("").ID, v.submit("").ID}
	for claimed, tries := 0, 0; claimed < 2; tries++ {
		n, err := v.w.RunOnce(context.Background())
		if err != nil || tries == 5 {
			t.Fatalf("claimed %d of 2 (err %v)", claimed, err)
		}
		claimed += n
	}
	for _, id := range ids {
		if got := v.get(id); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
			t.Fatalf("action = %+v", got)
		}
	}
	executes, issued := 0, 0
	for _, e := range v.audit() {
		switch e["path"] {
		case "/v1/execute":
			executes++
			if e["principal"] != "oauth:eacp-pkjwt" {
				t.Fatalf("an execute used principal %v, not a private_key_jwt token", e["principal"])
			}
		case "/oauth/token":
			if e["outcome"] == "token_issued" {
				issued++
				if jti, _ := e["assertion_jti"].(string); jti == "" {
					t.Fatalf("the issuance audit lacks the assertion's jti: %v", e)
				}
			}
		}
	}
	if executes != 2 || issued != 1 {
		t.Fatalf("%d executes and %d issued tokens, want 2 and one reused", executes, issued)
	}
	v.mu.Lock()
	values := append([]string{keyPEM, strings.Split(strings.TrimSpace(keyPEM), "\n")[1]}, v.tokens...)
	v.mu.Unlock()
	v.assertNotPersisted(values...)
}

func TestATokenShorterThanTheCallNeverDispatches(t *testing.T) {
	v := newJIT(t, 5*time.Second, 100) // the call needs 100 ms + 30 s of token life
	view := v.submit("")
	v.run(1)
	got := v.get(view.ID)
	if got.State != "QUEUED" || got.StateReason != "credential lifetime shorter than the call" || got.AttemptCount != 0 {
		t.Fatalf("action = %+v", got)
	}
	for _, e := range v.audit() {
		if e["path"] == "/v1/execute" {
			t.Fatal("a call was made with a token that could expire during it")
		}
	}
}

// A call longer than any token the IdP issues must not hold up the shorter
// calls on the same binding, nor make the worker mint over and over.
func TestALongCallDoesNotHoldUpShortCallsOnTheSameBinding(t *testing.T) {
	v := newJIT(t, 5*time.Minute, 100)
	long := v.submitTool("erp.create_po_eventual", "") // needs 300 s + 30 s of token life
	v.run(1)
	if got := v.get(long.ID); got.State != "QUEUED" || got.StateReason != "credential lifetime shorter than the call" {
		t.Fatalf("long action = %+v", got)
	}
	if len(v.secrets.Available()) != 1 {
		t.Fatal("a token too short for one call suspended the whole binding")
	}
	for range 3 {
		short := v.submit("")
		v.run(1)
		if got := v.get(short.ID); got.State != "SUCCEEDED" {
			t.Fatalf("short action = %+v", got)
		}
	}
	if got := v.get(long.ID); got.State != "QUEUED" || got.AttemptCount != 0 {
		t.Fatalf("long action = %+v", got)
	}
	if v.mints.Load() != 1 {
		t.Fatalf("%d mints: the long call made the worker mint again", v.mints.Load())
	}
}

// echoConnector is a careless connector: its result echoes the credential it
// was given. Before returning, the binding's cached token rotates (here by a
// rejection), so the store no longer lists the value that was sent.
type echoConnector struct{ secrets *worker.SecretStore }

func (e echoConnector) Execute(_ context.Context, c worker.Call) worker.Result {
	e.secrets.Rejected(c.TenantID, "erp-jit", c.Secret)
	return worker.Result{Outcome: worker.Ambiguous, ErrorClass: c.Secret.Reveal(), ExternalReference: c.Secret.Reveal()}
}

func (e echoConnector) Lookup(context.Context, worker.LookupCall) worker.LookupResult {
	return worker.LookupResult{Status: worker.LookupUnknown}
}

func TestTheTokenSentIsScrubbedEvenAfterTheCacheRotates(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	ctx := context.Background()
	w, err := worker.New(v.f.App, worker.Options{ID: "echo", Lease: 5 * time.Second, Log: v.logger,
		Connectors: map[string]worker.Connector{"http": echoConnector{v.secrets}}, Secrets: v.secrets,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	view := v.submit("")
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("claimed %d, %v", n, err)
	}
	v.mu.Lock()
	sent := v.tokens[0]
	v.mu.Unlock()
	for _, sql := range []string{
		`SELECT count(*) FROM eacp.actions WHERE to_jsonb(actions)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.action_attempts WHERE to_jsonb(action_attempts)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.outbox_events WHERE to_jsonb(outbox_events)::text LIKE '%' || $1 || '%'`,
	} {
		var count int
		if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, sent).Scan(&count)
		}); err != nil || count != 0 {
			t.Fatalf("the token sent was stored after the cache rotated (count=%d err=%v): %s", count, err, sql)
		}
	}
	if got := v.get(view.ID); strings.Contains(got.ExternalReference+got.StateReason, sent) {
		t.Fatal("the token sent reaches the action API")
	}
}

func TestAFailingTokenEndpointWithholdsClaimsUntilTheBackOffPasses(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	v.failing.Store(true)
	view := v.submit("")
	v.run(1)
	if got := v.get(view.ID); got.State != "QUEUED" || got.StateReason != "credential unavailable" || got.AttemptCount != 0 {
		t.Fatalf("action = %+v", got)
	}
	mints := v.mints.Load()
	for range 3 {
		v.run(0)
	}
	if v.mints.Load() != mints {
		t.Fatal("the token endpoint was called during the back-off")
	}
	v.failing.Store(false)
	v.clock.add(2 * time.Second)
	v.run(1)
	if got := v.get(view.ID); got.State != "SUCCEEDED" {
		t.Fatalf("action = %+v", got)
	}
}

func TestARejectedTokenIsReplacedOnTheNextAttempt(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	v.refuse.Store(true) // the ERP refuses the token, as after an IdP revocation
	refused := v.submit("")
	v.run(1)
	if got := v.get(refused.ID); got.AttemptCount != 1 {
		t.Fatalf("refused action = %+v", got)
	}
	v.refuse.Store(false)
	next := v.submit("")
	v.run(1)
	if got := v.get(next.ID); got.State != "SUCCEEDED" {
		t.Fatalf("next action = %+v", got)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.tokens) != 2 || v.tokens[0] == v.tokens[1] {
		t.Fatalf("a refused token was reused (%d issued)", len(v.tokens))
	}
}

func TestTheReconcilerLooksUpWithAMintedToken(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	ctx := context.Background()
	view := v.submit("execute_then_timeout")
	v.run(1)
	if got := v.get(view.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("action = %+v", got)
	}
	r, err := worker.NewReconciler(v.f.App, worker.ReconcilerOptions{ID: "jit-r", Lease: 5 * time.Second,
		Connectors: map[string]worker.Connector{"http": connector.NewHTTP()}, Secrets: v.secrets,
		MaxAttempts: 3, Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := v.get(view.ID)
		if got.State == "SUCCEEDED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("action = %+v", got)
		}
		if got.NextReconcileAt != nil {
			time.Sleep(time.Until(*got.NextReconcileAt) + 20*time.Millisecond)
		}
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	lookups := 0
	for _, e := range v.audit() {
		if e["path"] == "/v1/operations/{key}" {
			lookups++
			if e["principal"] != "oauth:eacp-worker" {
				t.Fatalf("a lookup used principal %v", e["principal"])
			}
		}
	}
	if lookups == 0 {
		t.Fatal("no lookup reached the ERP")
	}
}

func TestTheReconcilerWaitsWhileTheCredentialIsUnavailable(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	ctx := context.Background()
	view := v.submit("execute_then_timeout")
	v.run(1)
	v.failing.Store(true)
	v.clock.add(time.Hour) // the worker's token expired
	r, err := worker.NewReconciler(v.f.App, worker.ReconcilerOptions{ID: "jit-r", Lease: 5 * time.Second,
		Connectors: map[string]worker.Connector{"http": connector.NewHTTP()}, Secrets: v.secrets,
		MaxAttempts: 3, Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.get(view.ID); got.NextReconcileAt != nil {
		time.Sleep(time.Until(*got.NextReconcileAt) + 20*time.Millisecond)
	}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ev, err := v.engine.Evidence(ctx, v.f.Tenant, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Checks) != 0 {
		t.Fatalf("an unavailable credential recorded %d reconciliation checks", len(ev.Checks))
	}
	if got := v.get(view.ID); got.State != "UNKNOWN_OUTCOME" && got.State != "RECONCILING" {
		t.Fatalf("action = %+v", got)
	}
}
