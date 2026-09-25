// Package api is the control plane's HTTP API: registry, identity and
// capability (ADR-003), policies and approvals (ADR-002/005), the Action
// API (ADR-004) and budgets (ADR-012). Every request is authenticated with an
// API key; the handler checks the caller's role for a fast, friendly 403,
// and the database enforces every rule again underneath.
package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/action"
	"eacp/internal/approval"
	"eacp/internal/audit"
	"eacp/internal/budget"
	"eacp/internal/bundle"
	"eacp/internal/finops"
	"eacp/internal/fleet"
	"eacp/internal/governance"
	"eacp/internal/identity"
	"eacp/internal/incident"
	"eacp/internal/kill"
	"eacp/internal/registry"
	"eacp/internal/release"
	"eacp/internal/storage"
)

const maxBody = 1 << 20

// Server serves the API.
type Server struct {
	pool *pgxpool.Pool
	reg  *registry.Service
	gov  *governance.Store
	appr *approval.Service
	log  *slog.Logger

	budgets *budget.Service
	kills   *kill.Service
	fleet   *fleet.Service
	bundles *bundle.Service
	finops  *finops.Service

	actions   *action.Engine
	releases  *release.Service
	incidents *incident.Service
}

// New returns a Server using pool (connected as the application role).
func New(pool *pgxpool.Pool, log *slog.Logger) *Server {
	local := governance.LocalProvider{InstanceID: "controlplane-api"}
	return &Server{pool: pool, reg: registry.New(pool), gov: governance.NewStore(pool),
		appr: approval.New(pool), log: log, budgets: budget.New(pool), kills: kill.New(pool),
		fleet: fleet.New(pool), finops: finops.New(pool), bundles: bundle.New(pool), incidents: incident.New(pool),
		actions:  action.New(pool, action.Options{Provider: local, Log: log}),
		releases: release.New(pool, release.Options{Provider: local, Log: log})}
}

// Role sets used by routes.
var (
	anyPrincipal     []string // nil: any authenticated principal
	admin            = []string{"admin"}
	editor           = []string{"registry_editor"}
	approver         = []string{"registry_approver"}
	editorOrApprover = []string{"registry_editor", "registry_approver"}
	containment      = []string{"operator", "registry_approver"}
	auditor          = []string{"auditor"}
	policyAdmin      = []string{"admin"}
	actionApprover   = []string{"approver"}
	actionReader     = []string{"operator", "auditor"}
	operator         = []string{"operator"}
)

// Register mounts every route on mux.
func (s *Server) Register(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("GET /v1/me", p(anyPrincipal, s.me))

	mux.Handle("POST /v1/principals", p(admin, s.createPrincipal))
	mux.Handle("POST /v1/principals/{id}/disable", p(admin, s.disablePrincipal))
	mux.Handle("POST /v1/role-grants", p(admin, s.proposeRole))
	mux.Handle("POST /v1/role-grants/{id}/approve", p(admin, s.approveRole))
	mux.Handle("POST /v1/role-grants/{id}/revoke", p(admin, s.revokeRole))
	mux.Handle("POST /v1/groups", p(admin, s.createGroup))
	mux.Handle("POST /v1/groups/{id}/members", p(admin, s.addMember))
	mux.Handle("POST /v1/group-memberships/{id}/remove", p(admin, s.removeMember))

	mux.Handle("POST /v1/credentials", p([]string{"admin", "registry_editor", "registry_approver"}, s.proposeCredential))
	mux.Handle("POST /v1/credentials/{id}/approve", p([]string{"admin", "registry_approver"}, s.approveCredential))
	mux.Handle("POST /v1/credentials/{id}/revoke", p([]string{"admin", "registry_approver", "operator"}, s.revokeCredential))

	mux.Handle("POST /v1/agents", p(editor, s.registerAgent))
	mux.Handle("GET /v1/agents", p(anyPrincipal, s.listAgents))
	mux.Handle("GET /v1/agents/{ref}", p(anyPrincipal, s.getAgent))
	mux.Handle("POST /v1/agents/{id}/versions", p(editor, s.registerVersion))
	mux.Handle("POST /v1/agent-versions/{id}/allowlists", p(editorOrApprover, s.proposeAllowlist))
	mux.Handle("POST /v1/agent-versions/{id}/allowlist", p(approver, s.activateAllowlist))
	mux.Handle("POST /v1/agent-versions/{id}/transitions", p(containment, s.transitionVersion))

	mux.Handle("POST /v1/connectors", p(editor, s.registerConnector))
	mux.Handle("GET /v1/connectors", p(anyPrincipal, s.listConnectors))
	mux.Handle("POST /v1/connectors/{id}/tools", p(editor, s.registerTool))
	mux.Handle("GET /v1/connectors/{id}/circuit", p(actionReader, s.connectorCircuit))
	mux.Handle("POST /v1/connectors/{id}/circuit/disable", p(operator, s.switchCircuit(true)))
	mux.Handle("POST /v1/connectors/{id}/circuit/enable", p(operator, s.switchCircuit(false)))
	mux.Handle("POST /v1/tools/{id}/contracts", p(editorOrApprover, s.proposeContract))
	mux.Handle("POST /v1/tools/{id}/contract", p(approver, s.activateContract))
	mux.Handle("POST /v1/tool-contracts/{id}/revoke", p(containment, s.revokeContract))

	mux.Handle("GET /v1/audit/verify", p(auditor, s.verifyAudit))
	mux.Handle("POST /v1/policies", p(policyAdmin, s.createPolicy))
	mux.Handle("POST /v1/policies/{id}/activate", p(policyAdmin, s.activatePolicy))
	mux.Handle("GET /v1/policies/current", p(policyAdmin, s.currentPolicy))
	mux.Handle("GET /v1/approvals/{id}", p(actionApprover, s.getApproval))
	mux.Handle("GET /v1/approvals", p(actionApprover, s.listApprovals))
	mux.Handle("POST /v1/approvals/{id}/votes", p(actionApprover, s.voteApproval))

	mux.Handle("POST /v1/actions", s.agent(s.submitAction))
	mux.Handle("GET /v1/actions/{id}", s.either(actionReader, s.getAction))
	mux.Handle("POST /v1/actions/{id}/cancel", s.either(anyPrincipal, s.cancelAction))
	mux.Handle("GET /v1/actions", p(actionReader, s.listActions))
	mux.Handle("GET /v1/actions/{id}/evidence", p(actionReader, s.actionEvidence))
	mux.Handle("POST /v1/actions/{id}/resolutions", p(operator, s.resolveAction))
	mux.Handle("POST /v1/actions/{id}/resolutions/{rid}/confirm", p(operator, s.decideResolution(true)))
	mux.Handle("POST /v1/actions/{id}/resolutions/{rid}/withdraw", p(operator, s.decideResolution(false)))

	s.registerBudget(mux)
	s.registerMCP(mux)
	s.registerDependency(mux)
	s.registerKill(mux)
	s.registerFleet(mux)
	s.registerBundles(mux)
	s.registerFinOps(mux)
	s.registerRelease(mux)
	s.registerIncidents(mux)

	mux.Handle("GET /v1/agent/self", s.agent(s.agentSelf))
	mux.Handle("POST /v1/agent/capability-check", s.agent(s.capabilityCheck))
}

// ---------------------------------------------------------------- plumbing

type handler func(w http.ResponseWriter, r *http.Request, c identity.Caller) error

// authenticate returns the caller or writes the uniform 401.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (identity.Caller, bool) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := identity.Authenticate(r.Context(), s.pool, strings.TrimSpace(key))
	if !ok || err != nil {
		reason := "missing_bearer"
		var ae *identity.AuthError
		if ok && errors.As(err, &ae) {
			reason = ae.Reason
		} else if ok && err != nil {
			s.log.ErrorContext(r.Context(), "authentication error", "err", err)
			reason = "internal"
		}
		attrs := []any{"reason", reason, "path", r.URL.Path}
		if pk, perr := identity.ParseKey(strings.TrimSpace(key)); perr == nil {
			attrs = append(attrs, "credential", pk.CredentialID.String(), "tenant", pk.TenantID.String())
		}
		s.log.WarnContext(r.Context(), "authentication failed", attrs...)
		w.Header().Set("WWW-Authenticate", `Bearer realm="eacp"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return identity.Caller{}, false
	}
	return c, true
}

// principal wraps h for principal keys holding one of roles (nil: any).
func (s *Server) principal(roles []string, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if c.Kind != identity.KindPrincipal || (roles != nil && !c.HasRole(roles...)) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		s.finish(w, r, h(w, r, c))
	})
}

// agent wraps h for agent-runtime keys.
func (s *Server) agent(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if c.Kind != identity.KindAgent {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		s.finish(w, r, h(w, r, c))
	})
}

type badRequest struct{ msg string }

func (e badRequest) Error() string { return e.msg }

func (s *Server) finish(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	var br badRequest
	var be *bundle.Error
	var re *registry.Error
	switch {
	case errors.As(err, &br):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid", "detail": br.msg})
	case errors.As(err, &be):
		writeJSON(w, bundleStatus(be.Code), be)
	case errors.As(err, &re):
		code := map[error]int{
			registry.ErrForbidden: http.StatusForbidden, registry.ErrConflict: http.StatusConflict,
			registry.ErrNotFound: http.StatusNotFound, registry.ErrInvalid: http.StatusBadRequest,
		}[re.Kind]
		writeJSON(w, code, map[string]string{"error": re.Kind.Error(), "detail": re.Msg})
	default:
		s.log.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads a strict JSON body into v. An empty body leaves v unchanged.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return badRequest{fmt.Sprintf("body: %v", err)}
	}
	return nil
}

func pathID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, badRequest{name + " must be a UUID"}
	}
	return id, nil
}

func actor(c identity.Caller) registry.Actor {
	return registry.Actor{TenantID: c.TenantID, PrincipalID: c.PrincipalID}
}

func noContent(w http.ResponseWriter, err error) error {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
	}
	return err
}

func created(w http.ResponseWriter, v any, err error) error {
	if err == nil {
		writeJSON(w, http.StatusCreated, v)
	}
	return err
}

type reasonBody struct {
	Reason string `json:"reason"`
}

type idBody struct {
	ID uuid.UUID `json:"id"`
}

// -------------------------------------------------------------- principals

func (s *Server) me(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	roles := c.Roles
	if roles == nil {
		roles = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": c.TenantID, "principal_id": c.PrincipalID, "credential_id": c.CredentialID, "roles": roles})
	return nil
}

func (s *Server) createPrincipal(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in registry.NewPrincipal
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.reg.CreatePrincipal(r.Context(), actor(c), in)
	return created(w, p, err)
}

func (s *Server) disablePrincipal(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.reg.DisablePrincipal(r.Context(), actor(c), id, in.Reason))
}

func (s *Server) proposeRole(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		PrincipalID uuid.UUID `json:"principal_id"`
		Role        string    `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	id, err := s.reg.ProposeRole(r.Context(), actor(c), in.PrincipalID, in.Role)
	return created(w, idBody{id}, err)
}

func (s *Server) approveRole(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	return noContent(w, s.reg.ApproveRole(r.Context(), actor(c), id))
}

func (s *Server) revokeRole(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.reg.RevokeRole(r.Context(), actor(c), id, in.Reason))
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Weight      int    `json:"schedule_weight,omitempty"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Weight == 0 {
		in.Weight = 1
	}
	id, err := s.reg.CreateGroupWeighted(r.Context(), actor(c), in.Name, in.DisplayName, in.Weight)
	return created(w, idBody{id}, err)
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	group, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		PrincipalID uuid.UUID `json:"principal_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	id, err := s.reg.AddMember(r.Context(), actor(c), group, in.PrincipalID)
	return created(w, idBody{id}, err)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.reg.RemoveMember(r.Context(), actor(c), id, in.Reason))
}

// ------------------------------------------------------------- credentials

func (s *Server) proposeCredential(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		ID             uuid.UUID `json:"id"`
		Kind           string    `json:"kind"`
		PrincipalID    uuid.UUID `json:"principal_id"`
		AgentVersionID uuid.UUID `json:"agent_version_id"`
		Hash           string    `json:"hash"` // hex SHA-256 of the secret
		ExpiresInDays  int       `json:"expires_in_days"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	hash, err := hex.DecodeString(in.Hash)
	if err != nil || len(hash) != 32 {
		return badRequest{"hash must be 64 hex characters (SHA-256)"}
	}
	if in.ExpiresInDays < 1 || in.ExpiresInDays > 90 {
		return badRequest{"expires_in_days must be between 1 and 90"}
	}
	// The expiry is computed against the server clock; the database checks
	// it again against its own.
	err = s.reg.ProposeCredential(r.Context(), actor(c), registry.NewCredential{
		ID: in.ID, Kind: identity.Kind(in.Kind), PrincipalID: in.PrincipalID, AgentVersionID: in.AgentVersionID,
		Hash: hash, ExpiresAt: time.Now().Add(time.Duration(in.ExpiresInDays) * 24 * time.Hour).Add(-time.Minute),
	})
	return created(w, idBody{in.ID}, err)
}

func (s *Server) approveCredential(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	return noContent(w, s.reg.ApproveCredential(r.Context(), actor(c), id))
}

func (s *Server) revokeCredential(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.reg.RevokeCredential(r.Context(), actor(c), id, in.Reason))
}

// ------------------------------------------------------------------ agents

func (s *Server) registerAgent(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in registry.NewAgent
	if err := decode(r, &in); err != nil {
		return err
	}
	a, err := s.reg.RegisterAgent(r.Context(), actor(c), in)
	return created(w, a, err)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	agents, err := s.reg.ListAgents(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
	}
	return err
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	d, err := s.reg.GetAgent(r.Context(), actor(c), r.PathValue("ref"))
	if err == nil {
		writeJSON(w, http.StatusOK, d)
	}
	return err
}

func (s *Server) registerVersion(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	agentID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in registry.NewVersion
	if err := decode(r, &in); err != nil {
		return err
	}
	v, err := s.reg.RegisterVersion(r.Context(), actor(c), agentID, in)
	return created(w, v, err)
}

func (s *Server) proposeAllowlist(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	versionID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Tools []string `json:"tools"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	id, err := s.reg.ProposeAllowlist(r.Context(), actor(c), versionID, in.Tools)
	return created(w, idBody{id}, err)
}

func (s *Server) activateAllowlist(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	versionID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		AllowlistID uuid.UUID `json:"allowlist_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.reg.ActivateAllowlist(r.Context(), actor(c), versionID, in.AllowlistID))
}

func (s *Server) transitionVersion(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	versionID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		To     registry.State `json:"to"`
		Reason string         `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.reg.TransitionVersion(r.Context(), actor(c), versionID, in.To, in.Reason))
}

// -------------------------------------------------------------- connectors

func (s *Server) registerConnector(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in registry.NewConnector
	if err := decode(r, &in); err != nil {
		return err
	}
	conn, err := s.reg.RegisterConnector(r.Context(), actor(c), in)
	return created(w, conn, err)
}

func (s *Server) listConnectors(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	conns, err := s.reg.ListConnectors(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"connectors": conns})
	}
	return err
}

// connectorCircuit is GET /v1/connectors/{id}/circuit (ADR-022 §3).
func (s *Server) connectorCircuit(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	circuit, err := s.reg.ConnectorCircuit(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, circuit)
	}
	return err
}

// switchCircuit is POST /v1/connectors/{id}/circuit/disable and /enable: an
// operator stops or resumes new dispatch to a connector, with a reason.
func (s *Server) switchCircuit(disabled bool) func(http.ResponseWriter, *http.Request, identity.Caller) error {
	return func(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in reasonBody
		if err := decode(r, &in); err != nil {
			return err
		}
		circuit, err := s.reg.SetConnectorDisabled(r.Context(), actor(c), id, disabled, in.Reason)
		if err == nil {
			writeJSON(w, http.StatusOK, circuit)
		}
		return err
	}
}

func (s *Server) registerTool(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	connectorID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	id, err := s.reg.RegisterTool(r.Context(), actor(c), connectorID, in.Name)
	return created(w, idBody{id}, err)
}

func (s *Server) proposeContract(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	toolID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in registry.Contract
	if err := decode(r, &in); err != nil {
		return err
	}
	id, err := s.reg.ProposeContract(r.Context(), actor(c), toolID, in)
	return created(w, idBody{id}, err)
}

func (s *Server) activateContract(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	toolID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		ContractID uuid.UUID `json:"contract_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.reg.ActivateContract(r.Context(), actor(c), toolID, in.ContractID))
}

func (s *Server) revokeContract(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.reg.RevokeContract(r.Context(), actor(c), id, in.Reason))
}

// ------------------------------------------------------------------- audit

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var res audit.Result
	err := storage.InTenantReadTx(r.Context(), s.pool, c.TenantID.String(), func(tx pgx.Tx) error {
		var err error
		res, err = audit.Verify(r.Context(), tx)
		return err
	})
	if errors.Is(err, audit.ErrChainBroken) {
		s.log.ErrorContext(r.Context(), "audit chain broken", "tenant", c.TenantID.String(), "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return nil
	}
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"valid": true, "count": res.Count, "head": hex.EncodeToString(res.Head)})
	}
	return err
}

// ------------------------------------------------------------ agent routes

func (s *Server) agentSelf(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": c.TenantID, "agent_id": c.AgentID, "agent_version_id": c.AgentVersionID, "state": c.AgentState})
	return nil
}

func (s *Server) capabilityCheck(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Tool string `json:"tool"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	var g registry.Grant
	var d registry.Denial
	err := storage.InTenantTx(r.Context(), s.pool, c.TenantID.String(), func(tx pgx.Tx) error {
		var err error
		g, d, err = registry.CheckCapability(r.Context(), tx, c.AgentVersionID, in.Tool)
		return err
	})
	if err != nil {
		return err
	}
	if d != "" {
		writeJSON(w, http.StatusOK, map[string]any{"allowed": false, "denial": d})
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"allowed": true, "tool_id": g.ToolID, "contract_id": g.ContractID, "contract_version": g.ContractVersion})
	return nil
}
