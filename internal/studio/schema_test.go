package studio_test

// Schema-level tests for Agent Studio (Phase 27a-1, ADR-033): every rule is
// enforced by PostgreSQL, so each test issues raw SQL as the application
// role, bypassing the Go layer.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
)

const (
	sqlForbidden  = "42501"
	sqlBadState   = "55000"
	sqlCheck      = "23514"
	sqlForeignKey = "23503"

	proposeGrantSQL = `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`
	saveSQL   = `SELECT eacp.studio_save($1, $2, $3, $4, $5, $6)`
	decideSQL = `SELECT eacp.studio_decide($1, $2, $3)`
	akSQL     = `INSERT INTO eacp.credentials (tenant_id, id, kind, agent_version_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), gen_random_uuid(), 'ak', $1, decode(repeat('ab', 32), 'hex'),
		        now() + interval '90 days') RETURNING id`
)

// example is the leave-balance agent of the spec (section 3.2), canonical.
const example = `{"inputs":{"employee_id":{"max_length":64,"type":"string"}},"kind":"agent",` +
	`"limits":{"timeout_seconds":300},"schema_version":1,"steps":[{"id":"lookup","kind":"tool_call",` +
	`"operation":"lookup","payload":{"employee_id":"{{inputs.employee_id}}"},"resource":"leave_balance",` +
	`"target":"hr","tool":"hr-mcp.get_leave_balance","tool_schema_version":"1"},{"id":"answer","kind":"respond",` +
	`"text":"You have {{steps.lookup.output.structuredContent.days}} days of leave left."}]}`

func wantState(t *testing.T, err error, codes ...string) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want SQLSTATE %v", err, codes)
	}
	if !slices.Contains(codes, pgErr.Code) {
		t.Fatalf("SQLSTATE %s (%s), want %v", pgErr.Code, pgErr.Message, codes)
	}
	return pgErr
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fix is the registry fixture plus Studio's cast:
//
//	stella, sid  studio_author, members of hr and finance respectively
//	abe          studio_author and registry_approver, member of hr
//	rt           service studio_runtime
type fix struct {
	*registrytest.Fixture
	t                *testing.T
	hr, finance      uuid.UUID
	balance, erpRead registrytest.Tooling
}

func newFix(t *testing.T) *fix {
	t.Helper()
	f := &fix{Fixture: registrytest.New(t), t: t}
	f.AddPrincipal(t, "stella", "human", "studio_author")
	f.AddPrincipal(t, "sid", "human", "studio_author")
	f.AddPrincipal(t, "abe", "human", "studio_author", "registry_approver")
	f.AddPrincipal(t, "rt", "service", "studio_runtime")
	group := func(name string, members ...string) uuid.UUID {
		g := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
			VALUES (eacp.current_tenant_id(), $1, $1) RETURNING id`, name)
		for _, m := range members {
			f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
				VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, g, f.P[m])
		}
		return g
	}
	f.hr = group("hr", "stella", "abe")
	f.finance = group("finance", "sid")
	f.balance = f.ActiveTool(t, "hr-mcp", "get_leave_balance")
	f.erpRead = f.ActiveTool(t, "erp", "read")
	return f
}

// save saves definition as actor: a new agent named name when agent is nil.
func (f *fix) save(actor string, agent any, name string, def string) (uuid.UUID, error) {
	var dept, display, desc any
	if agent == nil {
		dept, display, desc = f.hr, "Leave balance", "Answers how many days of leave are left."
	} else {
		name = ""
	}
	var n any
	if name != "" {
		n = name
	}
	return f.TryID(actor, saveSQL, agent, n, display, desc, dept, def)
}

func (f *fix) mustSave(actor string, agent any, name, def string) uuid.UUID {
	f.t.Helper()
	v, err := f.save(actor, agent, name, def)
	ok(f.t, err)
	return v
}

// agentOf returns version's agent id.
func (f *fix) agentOf(version uuid.UUID) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	f.owner(func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT agent_id FROM eacp.agent_versions WHERE id = $1`, version).Scan(&id)
	})
	return id
}

// owner runs fn as the schema owner in the fixture tenant.
func (f *fix) owner(fn func(pgx.Tx) error) {
	f.t.Helper()
	ok(f.t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), fn))
}

func (f *fix) scalar(sql string, args ...any) string {
	f.t.Helper()
	var s string
	f.owner(func(tx pgx.Tx) error { return tx.QueryRow(context.Background(), sql, args...).Scan(&s) })
	return s
}

func (f *fix) decide(actor string, version uuid.UUID, approve bool, reason string) error {
	return f.Exec(actor, decideSQL, version, approve, reason)
}

// ---------------------------------------------------------------- roles

func TestStudioRolesAreGrantedByTwoAdmins(t *testing.T) {
	f := newFix(t)
	g := f.ID(t, "alice", proposeGrantSQL, f.P["carol"], "studio_author")
	wantState(t, f.Exec("alice", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, g), sqlForbidden)
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, g))

	svc := f.ID(t, "alice", `INSERT INTO eacp.principals (tenant_id, kind, name, display_name)
		VALUES (eacp.current_tenant_id(), 'service', 'runtime-2', 'Runtime') RETURNING id`)
	g = f.ID(t, "alice", proposeGrantSQL, svc, "studio_runtime")
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, g))
}

func TestStudioRuntimeIsHeldAlone(t *testing.T) {
	f := newFix(t)
	// Another role for the runtime, and the runtime role beside another.
	_, err := f.TryID("alice", proposeGrantSQL, f.P["rt"], "auditor")
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("alice", proposeGrantSQL, f.P["ci"], "studio_runtime")
	wantState(t, err, sqlForbidden)

	// A pending grant counts.
	svc := f.ID(t, "alice", `INSERT INTO eacp.principals (tenant_id, kind, name, display_name)
		VALUES (eacp.current_tenant_id(), 'service', 'runtime-3', 'Runtime') RETURNING id`)
	pending := f.ID(t, "alice", proposeGrantSQL, svc, "auditor")
	_, err = f.TryID("bob", proposeGrantSQL, svc, "studio_runtime")
	wantState(t, err, sqlForbidden)

	// A revoked grant does not.
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = 'wrong role' WHERE id = $1`, pending))
	f.ID(t, "alice", proposeGrantSQL, svc, "studio_runtime")
}

func TestStudioRolesKeepTheirPrincipalKind(t *testing.T) {
	f := newFix(t)
	_, err := f.TryID("alice", proposeGrantSQL, f.P["carol"], "studio_runtime")
	wantState(t, err, sqlForbidden)
	svc := f.ID(t, "alice", `INSERT INTO eacp.principals (tenant_id, kind, name, display_name)
		VALUES (eacp.current_tenant_id(), 'service', 'runtime-4', 'Runtime') RETURNING id`)
	_, err = f.TryID("alice", proposeGrantSQL, svc, "studio_author")
	wantState(t, err, sqlForbidden)
}

// ---------------------------------------------------------------- saving

func TestAStudioSaveCreatesTheAgentItsVersionAndItsAllowlist(t *testing.T) {
	f := newFix(t)
	v := f.mustSave("stella", nil, "leave-bot", example)
	agent := f.agentOf(v)

	if got := f.scalar(`SELECT concat_ws(' ', a.environment, a.risk_class, a.owner_principal_id = $2,
			a.created_by = $2, s.department_group_id = $3, s.created_by = $2)
		FROM eacp.agents a JOIN eacp.studio_agents s ON s.tenant_id = a.tenant_id AND s.id = a.id
		WHERE a.id = $1`, agent, f.P["stella"], f.hr); got != "production high t t t t" {
		t.Fatalf("agent = %q", got)
	}
	sum := sha256.Sum256([]byte(example))
	digest := hex.EncodeToString(sum[:])
	if got := f.scalar(`SELECT concat_ws(' ', v.version, v.state, v.runtime, v.code_ref, (v.created_by = $2)::text,
			s.digest, s.capability::text, (s.decision IS NULL)::text, (s.definition = $3)::text)
		FROM eacp.agent_versions v JOIN eacp.studio_versions s ON s.tenant_id = v.tenant_id AND s.id = v.id
		WHERE v.id = $1`, v, f.P["stella"], example); got != fmt.Sprintf(
		"1 REGISTERED studio studio:%s true %s {hr-mcp.get_leave_balance} true true", digest, digest) {
		t.Fatalf("version = %q", got)
	}
	if got := f.scalar(`SELECT concat_ws(' ', (tool_ids = ARRAY[$2]::uuid[])::text, (created_by = $3)::text)
		FROM eacp.agent_allowlists WHERE agent_version_id = $1`, v, f.balance.Tool, f.P["stella"]); got != "true true" {
		t.Fatalf("allowlist = %q", got)
	}
	// Journaled as the author.
	if got := f.scalar(`SELECT count(*)::text FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->>'action' = 'studio_versions.insert'
		  AND convert_from(payload, 'UTF8')::jsonb->'actor'->>'id' = $1::text`, f.P["stella"]); got != "1" {
		t.Fatalf("journaled saves = %s", got)
	}

	// The next version is numbered 2 and has its own allowlist.
	v2 := f.mustSave("stella", agent, "", example)
	if got := f.scalar(`SELECT version::text FROM eacp.agent_versions WHERE id = $1`, v2); got != "2" {
		t.Fatalf("version = %s", got)
	}
}

func TestOnlyAStudioAuthorInTheDepartmentSaves(t *testing.T) {
	f := newFix(t)
	for _, who := range []string{"erin", "carol", "rita", "rt"} {
		_, err := f.save(who, nil, "bot-"+who, example)
		wantState(t, err, sqlForbidden)
	}
	// sid is an author, but not in hr.
	_, err := f.save("sid", nil, "bot-sid", example)
	wantState(t, err, sqlForbidden)
	// A new agent names its department.
	_, err = f.TryID("stella", saveSQL, nil, "no-dept", "No department", "x", nil, example)
	wantState(t, err, sqlCheck)
}

func TestOnlyTheOwnerAddsAVersion(t *testing.T) {
	f := newFix(t)
	agent := f.agentOf(f.mustSave("stella", nil, "leave-bot", example))
	_, err := f.save("abe", agent, "", example)
	wantState(t, err, sqlForeignKey)
	// Not a Studio agent.
	plain := f.NewAgent(t, "plain")
	_, err = f.save("stella", plain.Agent, "", example)
	wantState(t, err, sqlForeignKey)
	// A new version carries only its definition.
	_, err = f.TryID("stella", saveSQL, agent, "renamed", nil, nil, nil, example)
	wantState(t, err, sqlCheck)
}

func TestAStudioAuthorWritesNoRegistryRowDirectly(t *testing.T) {
	f := newFix(t)
	v := f.mustSave("stella", nil, "leave-bot", example)
	agent := f.agentOf(v)
	direct := []string{
		`INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
			VALUES (eacp.current_tenant_id(), 'direct', 'Direct', 'production', 'high', '` + f.P["stella"].String() + `')`,
		`INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
			VALUES (eacp.current_tenant_id(), '` + agent.String() + `', 'studio', 'studio:x')`,
		`INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
			VALUES (eacp.current_tenant_id(), '` + v.String() + `', ARRAY['` + f.erpRead.Tool.String() + `']::uuid[])`,
		`INSERT INTO eacp.studio_agents (tenant_id, id, department_group_id, description, created_by)
			VALUES (eacp.current_tenant_id(), '` + agent.String() + `', '` + f.hr.String() + `', 'x', '` + f.P["stella"].String() + `')`,
		`INSERT INTO eacp.studio_versions (tenant_id, id, agent_id, definition, digest, capability, created_by)
			VALUES (eacp.current_tenant_id(), '` + v.String() + `', '` + agent.String() + `', '{}', 'x', '{}', '` + f.P["stella"].String() + `')`,
		`UPDATE eacp.studio_versions SET decision = 'approved' WHERE id = '` + v.String() + `'`,
		`INSERT INTO eacp.studio_save_marks (tenant_id, xact) VALUES (eacp.current_tenant_id(), pg_current_xact_id())`,
	}
	for _, sql := range direct {
		wantState(t, f.Exec("stella", sql), sqlForbidden)
	}
	// Nor right after a save in the same transaction.
	ctx := context.Background()
	err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P["stella"]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, saveSQL, agent, nil, nil, nil, nil, example); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, direct[0])
		return err
	})
	wantState(t, err, sqlForbidden)
}

func TestTheRegistryCannotWidenAStudioAgent(t *testing.T) {
	f := newFix(t)
	v := f.mustSave("stella", nil, "leave-bot", example)
	agent := f.agentOf(v)
	// No version and no allowlist outside a Studio save, even for an editor.
	_, err := f.TryID("erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:abc') RETURNING id`, agent)
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, v, []uuid.UUID{f.balance.Tool, f.erpRead.Tool})
	wantState(t, err, sqlForbidden)
	// No activation outside a decision.
	allowlist := f.scalar(`SELECT id::text FROM eacp.agent_allowlists WHERE agent_version_id = $1`, v)
	wantState(t, f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, allowlist, v), sqlForbidden)
}

// mutate returns example changed by fn.
func mutate(t *testing.T, fn func(d map[string]any)) string {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(example), &d); err != nil {
		t.Fatal(err)
	}
	fn(d)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func steps(d map[string]any) []any                { return d["steps"].([]any) }
func step(d map[string]any, i int) map[string]any { return steps(d)[i].(map[string]any) }

func TestTheDefinitionRules(t *testing.T) {
	f := newFix(t)
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := map[string]struct {
		def  string
		want string // a fragment of the message
	}{
		"not JSON":            {`{"kind":`, "JSON object"},
		"an array":            {`[]`, "JSON object"},
		"duplicate keys":      {`{"kind":"agent","kind":"agent"}`, "JSON object"},
		"too large":           {mutate(t, func(d map[string]any) { step(d, 1)["text"] = long(70000) }), "65536"},
		"schema version 2":    {mutate(t, func(d map[string]any) { d["schema_version"] = 2 }), "schema_version"},
		"schema version text": {mutate(t, func(d map[string]any) { d["schema_version"] = "1" }), "schema_version"},
		"kind":                {mutate(t, func(d map[string]any) { d["kind"] = "workflow" }), "kind"},
		"unknown key":         {mutate(t, func(d map[string]any) { d["schedule"] = "daily" }), "unknown"},
		"no limits":           {mutate(t, func(d map[string]any) { delete(d, "limits") }), "limits"},
		"timeout low":         {mutate(t, func(d map[string]any) { d["limits"] = map[string]any{"timeout_seconds": 9} }), "timeout_seconds"},
		"timeout high":        {mutate(t, func(d map[string]any) { d["limits"] = map[string]any{"timeout_seconds": 3601} }), "timeout_seconds"},
		"timeout fraction":    {mutate(t, func(d map[string]any) { d["limits"] = map[string]any{"timeout_seconds": 10.5} }), "timeout_seconds"},
		"limits unknown key":  {mutate(t, func(d map[string]any) { d["limits"] = map[string]any{"timeout_seconds": 60, "retries": 3} }), "limits"},
		"inputs not object":   {mutate(t, func(d map[string]any) { d["inputs"] = []any{} }), "inputs"},
		"eleven inputs": {mutate(t, func(d map[string]any) {
			in := map[string]any{}
			for i := range 11 {
				in[fmt.Sprintf("i%d", i)] = map[string]any{"type": "string", "max_length": 8}
			}
			d["inputs"] = in
		}), "inputs"},
		"input name": {mutate(t, func(d map[string]any) {
			d["inputs"] = map[string]any{"Emp": map[string]any{"type": "string", "max_length": 8}}
		}), "input"},
		"input type": {mutate(t, func(d map[string]any) {
			d["inputs"] = map[string]any{"employee_id": map[string]any{"type": "number", "max_length": 8}}
		}), "string"},
		"input max zero": {mutate(t, func(d map[string]any) {
			d["inputs"] = map[string]any{"employee_id": map[string]any{"type": "string", "max_length": 0}}
		}), "max_length"},
		"input max 1025": {mutate(t, func(d map[string]any) {
			d["inputs"] = map[string]any{"employee_id": map[string]any{"type": "string", "max_length": 1025}}
		}), "max_length"},
		"input extra key": {mutate(t, func(d map[string]any) {
			d["inputs"] = map[string]any{"employee_id": map[string]any{"type": "string", "max_length": 8, "default": "x"}}
		}), "input"},
		"no steps":        {mutate(t, func(d map[string]any) { d["steps"] = []any{} }), "steps"},
		"steps not array": {mutate(t, func(d map[string]any) { d["steps"] = map[string]any{} }), "steps"},
		"twenty-one steps": {mutate(t, func(d map[string]any) {
			s := []any{}
			for i := range 20 {
				c := map[string]any{}
				for k, v := range step(d, 0) {
					c[k] = v
				}
				c["id"] = fmt.Sprintf("s%d", i)
				s = append(s, c)
			}
			d["steps"] = append(s, step(d, 1))
		}), "steps"},
		"duplicate id":     {mutate(t, func(d map[string]any) { step(d, 1)["id"] = "lookup" }), "unique"},
		"bad id":           {mutate(t, func(d map[string]any) { step(d, 0)["id"] = "Look-up" }), "id"},
		"step kind":        {mutate(t, func(d map[string]any) { step(d, 0)["kind"] = "llm" }), "kind"},
		"respond not last": {mutate(t, func(d map[string]any) { d["steps"] = []any{step(d, 1), step(d, 0)} }), "respond"},
		"no respond":       {mutate(t, func(d map[string]any) { d["steps"] = []any{step(d, 0)} }), "respond"},
		"two responds": {mutate(t, func(d map[string]any) {
			r := map[string]any{"id": "early", "kind": "respond", "text": "hi"}
			d["steps"] = []any{r, step(d, 0), step(d, 1)}
		}), "respond"},
		"unknown tool":       {mutate(t, func(d map[string]any) { step(d, 0)["tool"] = "hr-mcp.nope" }), "unknown tool"},
		"tool reference":     {mutate(t, func(d map[string]any) { step(d, 0)["tool"] = "get_leave_balance" }), "tool"},
		"empty operation":    {mutate(t, func(d map[string]any) { step(d, 0)["operation"] = " " }), "operation"},
		"long target":        {mutate(t, func(d map[string]any) { step(d, 0)["target"] = long(129) }), "target"},
		"resource number":    {mutate(t, func(d map[string]any) { step(d, 0)["resource"] = 3 }), "resource"},
		"no schema version":  {mutate(t, func(d map[string]any) { delete(step(d, 0), "tool_schema_version") }), "tool_schema_version"},
		"payload not object": {mutate(t, func(d map[string]any) { step(d, 0)["payload"] = "x" }), "payload"},
		"step extra key":     {mutate(t, func(d map[string]any) { step(d, 0)["code"] = "print(1)" }), "unknown"},
		"respond extra key":  {mutate(t, func(d map[string]any) { step(d, 1)["tool"] = "erp.read" }), "unknown"},
		"long text":          {mutate(t, func(d map[string]any) { step(d, 1)["text"] = long(4097) }), "text"},
		"undeclared input":   {mutate(t, func(d map[string]any) { step(d, 1)["text"] = "{{inputs.nobody}}" }), "placeholder"},
		"later step": {mutate(t, func(d map[string]any) {
			step(d, 0)["payload"] = map[string]any{"x": "{{steps.answer.output}}"}
		}), "placeholder"},
		"own step": {mutate(t, func(d map[string]any) {
			step(d, 0)["payload"] = map[string]any{"x": "{{steps.lookup.output}}"}
		}), "placeholder"},
		"spaces":         {mutate(t, func(d map[string]any) { step(d, 1)["text"] = "{{ inputs.employee_id }}" }), "placeholder"},
		"expression":     {mutate(t, func(d map[string]any) { step(d, 1)["text"] = "{{inputs.employee_id | upper}}" }), "placeholder"},
		"nine deep":      {mutate(t, func(d map[string]any) { step(d, 1)["text"] = "{{steps.lookup.output.a.b.c.d.e.f.g.h.i}}" }), "placeholder"},
		"unclosed":       {mutate(t, func(d map[string]any) { step(d, 1)["text"] = "{{inputs.employee_id" }), "placeholder"},
		"in operation":   {mutate(t, func(d map[string]any) { step(d, 0)["operation"] = "{{inputs.employee_id}}" }), "placeholder"},
		"in payload key": {mutate(t, func(d map[string]any) { step(d, 0)["payload"] = map[string]any{"{{inputs.employee_id}}": "x"} }), "placeholder"},
		"escaped braces": {strings.Replace(example, `"You have {{`, `"You have {{inputs.nobody}} {{`, 1), "placeholder"},
	}
	for name, secret := range map[string]string{
		"eacp agent key": "eacp_ak_0011", "eacp principal key": "eacp_pk_0011", "pem": "-----BEGIN PRIVATE KEY-----",
		"aws key": "AKIAABCDEFGHIJKLMNOP", "bearer": "Bearer abc", "github": "ghp_abcdef", "slack": "xoxb-123",
		"openai": "sk-abcdefghijklmnopqrstuvwxyz",
	} {
		cases["secret "+name] = struct{ def, want string }{
			mutate(t, func(d map[string]any) { step(d, 0)["payload"] = map[string]any{"token": secret} }),
			"definition_contains_secret"}
	}
	cases["secret in a key"] = struct{ def, want string }{
		mutate(t, func(d map[string]any) { step(d, 0)["payload"] = map[string]any{"ghp_abcdef": "x"} }),
		"definition_contains_secret"}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.save("stella", nil, "bot-"+uuid.NewString()[:8], c.def)
			if e := wantState(t, err, sqlCheck); !strings.Contains(e.Message, c.want) {
				t.Fatalf("message %q, want %q", e.Message, c.want)
			}
		})
	}
	if n := f.scalar(`SELECT count(*)::text FROM eacp.studio_versions`); n != "0" {
		t.Fatalf("saved %s refused definitions", n)
	}
}

func TestTheCapabilityIsTheSortedDistinctTools(t *testing.T) {
	f := newFix(t)
	def := mutate(t, func(d map[string]any) {
		read := map[string]any{}
		for k, v := range step(d, 0) {
			read[k] = v
		}
		read["tool"], read["id"] = "erp.read", "read_a"
		again := map[string]any{}
		for k, v := range read {
			again[k] = v
		}
		again["id"] = "read_b"
		again["payload"] = map[string]any{"days": "{{steps.lookup.output.structuredContent.days}}", "n": 3}
		d["steps"] = []any{step(d, 0), read, again, step(d, 1)}
	})
	v := f.mustSave("stella", nil, "two-tools", def)
	if got := f.scalar(`SELECT capability::text FROM eacp.studio_versions WHERE id = $1`, v); got != "{erp.read,hr-mcp.get_leave_balance}" {
		t.Fatalf("capability = %s", got)
	}
	want := []uuid.UUID{f.balance.Tool, f.erpRead.Tool}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if got := f.scalar(`SELECT (tool_ids = $2)::text FROM eacp.agent_allowlists WHERE agent_version_id = $1`, v, want); got != "true" {
		t.Fatalf("allowlist differs from the capability")
	}
}

// -------------------------------------------------------------- decisions

func TestTheOwnerAndNonApproversCannotDecide(t *testing.T) {
	f := newFix(t)
	own := f.mustSave("abe", nil, "abes-bot", example)
	wantState(t, f.decide("abe", own, true, "looks fine"), sqlBadState)
	wantState(t, f.decide("abe", own, false, "changed my mind"), sqlBadState)
	for _, who := range []string{"erin", "stella", "otto", "rt"} {
		wantState(t, f.decide(who, own, true, "ok"), sqlForbidden)
	}
	wantState(t, f.decide("rita", own, true, " "), sqlCheck)
	wantState(t, f.decide("rita", uuid.New(), true, "ok"), sqlForeignKey)
	plain := f.NewAgent(t, "plain")
	wantState(t, f.decide("rita", plain.Version, true, "ok"), sqlForeignKey)
}

func TestApprovalActivatesTheVersionAndReplacesThePreviousOne(t *testing.T) {
	f := newFix(t)
	v1 := f.mustSave("stella", nil, "leave-bot", example)
	ok(t, f.decide("rita", v1, true, "tools are read-only"))
	if got := f.scalar(`SELECT concat_ws(' ', v.state, v.active_allowlist_id IS NOT NULL, v.state_changed_by = $2,
			s.decision, s.decided_by = $2, s.decision_reason)
		FROM eacp.agent_versions v JOIN eacp.studio_versions s ON s.tenant_id = v.tenant_id AND s.id = v.id
		WHERE v.id = $1`, v1, f.P["rita"]); got != "ACTIVE t t approved t tools are read-only" {
		t.Fatalf("v1 = %q", got)
	}
	wantState(t, f.decide("ravi", v1, false, "again"), sqlBadState)

	v2 := f.mustSave("stella", f.agentOf(v1), "", example)
	ok(t, f.decide("ravi", v2, true, "same tools"))
	if got := f.scalar(`SELECT string_agg(concat_ws(' ', version, state, state_reason), ', ' ORDER BY version)
		FROM eacp.agent_versions WHERE agent_id = $1`, f.agentOf(v1)); got != "1 RETIRED replaced by version 2, 2 ACTIVE same tools" {
		t.Fatalf("versions = %q", got)
	}
	// The decisions are journaled.
	if got := f.scalar(`SELECT count(*)::text FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->>'action' = 'studio_versions.update'
		  AND convert_from(payload, 'UTF8')::jsonb->'data' ? 'decision'`); got != "2" {
		t.Fatalf("journaled decisions = %s", got)
	}
}

func TestRejectionRetiresTheVersion(t *testing.T) {
	f := newFix(t)
	v := f.mustSave("stella", nil, "leave-bot", example)
	ok(t, f.decide("rita", v, false, "use the HR portal"))
	if got := f.scalar(`SELECT concat_ws(' ', v.state, v.state_reason, s.decision)
		FROM eacp.agent_versions v JOIN eacp.studio_versions s ON s.tenant_id = v.tenant_id AND s.id = v.id
		WHERE v.id = $1`, v); got != "RETIRED use the HR portal rejected" {
		t.Fatalf("rejected = %q", got)
	}
	wantState(t, f.decide("ravi", v, true, "overrule"), sqlBadState)
}

func TestOnlyAWaitingVersionIsApproved(t *testing.T) {
	f := newFix(t)
	v := f.mustSave("stella", nil, "leave-bot", example)
	ok(t, f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'QUARANTINED', state_reason = 'suspicious' WHERE id = $1`, v))
	wantState(t, f.decide("rita", v, true, "ok"), sqlBadState)

	// An agent with an open release is not changed by a decision.
	// Only an approved version has an allowlist, so the candidate is an
	// approved, suspended version beside the ACTIVE one.
	v1 := f.mustSave("stella", nil, "released-bot", example)
	agent := f.agentOf(v1)
	ok(t, f.decide("rita", v1, true, "ok"))
	ok(t, f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'pause' WHERE id = $1`, v1))
	v2 := f.mustSave("stella", agent, "", example)
	ok(t, f.decide("rita", v2, true, "ok"))
	f.ID(t, "erin", `INSERT INTO eacp.agent_releases (tenant_id, candidate_version_id, required_suites, reason)
		VALUES (eacp.current_tenant_id(), $1, '{safety}', 'try it') RETURNING id`, v1)
	v3 := f.mustSave("stella", agent, "", example)
	wantState(t, f.decide("ravi", v3, true, "ok"), sqlBadState)
	ok(t, f.decide("ravi", v3, false, "not during a release"))
}

// ------------------------------------------------------------ credentials

func TestTheRuntimeProposesCredentialsOnlyForApprovedStudioVersions(t *testing.T) {
	f := newFix(t)
	v := f.mustSave("stella", nil, "leave-bot", example)
	_, err := f.TryID("rt", akSQL, v)
	wantState(t, err, sqlForbidden)
	plain := f.ActiveAgent(t, "plain", f.erpRead.Tool)
	_, err = f.TryID("rt", akSQL, plain.Version)
	wantState(t, err, sqlForbidden)

	ok(t, f.decide("rita", v, true, "ok"))
	cred := f.ID(t, "rt", akSQL, v)
	approve := `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`
	wantState(t, f.Exec("rt", approve, cred), sqlForbidden)
	ok(t, f.Exec("ravi", approve, cred))

	// A rejected version gets none.
	r := f.mustSave("stella", nil, "rejected-bot", example)
	ok(t, f.decide("rita", r, false, "no"))
	_, err = f.TryID("rt", akSQL, r)
	wantState(t, err, sqlForbidden, sqlBadState)

	// The runtime does nothing else in the registry.
	for _, sql := range []string{
		`INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
			VALUES (eacp.current_tenant_id(), 'rt-agent', 'x', 'production', 'high', '` + f.P["rt"].String() + `')`,
		`INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
			VALUES (eacp.current_tenant_id(), 'rt-conn', 'http', 'http://x', 'x')`,
		`UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'x' WHERE id = '` + v.String() + `'`,
		`UPDATE eacp.credentials SET revoked_at = now(), revoke_reason = 'x' WHERE id = '` + cred.String() + `'`,
	} {
		wantState(t, f.Exec("rt", sql), sqlForbidden)
	}
	_, err = f.save("rt", nil, "rt-bot", example)
	wantState(t, err, sqlForbidden)
	wantState(t, f.decide("rt", r, true, "x"), sqlForbidden)
}
