package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/registry"
)

// MaxSteps is the most steps one change set may have (the ordinal CHECK of
// migration 00020).
const MaxSteps = 2000

// Document is a bundle's resolved desired state. eacpctl resolves targets
// and variables before sending it; the server accepts JSON only.
type Document struct {
	Principals map[string]Principal `json:"principals,omitempty"`
	Groups     map[string]Group     `json:"groups,omitempty"`
	Connectors map[string]Connector `json:"connectors,omitempty"`
	Agents     map[string]Agent     `json:"agents,omitempty"`
	Policy     *Policy              `json:"policy,omitempty"`
	Budgets    map[string]Budget    `json:"budgets,omitempty"`
	Prices     map[string]Price     `json:"prices,omitempty"`
	Imports    []Import             `json:"imports,omitempty"`
}

// Connector is a declared connector. SecretRef names a secret the worker
// holds; it is never a secret value.
type Connector struct {
	Protocol  string          `json:"protocol"`
	Endpoint  string          `json:"endpoint"`
	SecretRef string          `json:"secret_ref"`
	Tools     map[string]Tool `json:"tools,omitempty"`
}

// Tool is a declared tool. A tool of an MCP or A2A connector is discovered,
// never created; declaring it only manages its contract.
type Tool struct {
	Contract *registry.Contract `json:"contract,omitempty"`
}

// Owner names the agent's one owner: a principal or a group.
type Owner struct {
	Principal string `json:"principal,omitempty"`
	Group     string `json:"group,omitempty"`
}

// VersionSpec identifies the version the bundle wants.
type VersionSpec struct {
	Runtime string `json:"runtime"`
	CodeRef string `json:"code_ref"`
}

// Agent is a declared agent. State "ACTIVE" asks for the version to be
// activated; omitted, the bundle leaves the version's state alone.
type Agent struct {
	DisplayName string      `json:"display_name"`
	Environment string      `json:"environment"`
	RiskClass   string      `json:"risk_class"`
	Owner       Owner       `json:"owner"`
	Version     VersionSpec `json:"version"`
	Allowlist   []string    `json:"allowlist,omitempty"`
	State       string      `json:"state,omitempty"`
}

// Import adopts an existing object into the bundle without changing it.
type Import struct {
	To string    `json:"to"`
	ID uuid.UUID `json:"id"`
}

// Finding kinds. Orphans and unmanaged references are reported; every other
// kind blocks the plan.
const (
	KindInvalid             = "invalid"
	KindUnresolvedVariable  = "unresolved_variable"
	KindUnresolvedReference = "unresolved_reference"
	KindUnsupported         = "unsupported"
	KindUnmanaged           = "unmanaged"
	KindRequiresRelease     = "requires_release"
	KindContained           = "contained"
	KindOrphan              = "orphan"
	KindUnmanagedReference  = "unmanaged_reference"
	KindPending             = "pending"
	KindAdminFloor          = "admin_floor"
)

// Finding is something a plan reports about an address.
type Finding struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail"`
}

// Blocking reports whether the finding stops the plan from being recorded.
func (f Finding) Blocking() bool { return f.Kind != KindOrphan && f.Kind != KindUnmanagedReference }

func blocked(fs []Finding) bool { return slices.ContainsFunc(fs, Finding.Blocking) }

var (
	slugRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	toolNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	secretRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)
	endpointRE = regexp.MustCompile(`^https?://[^[:space:]]+$`)
	importRE   = regexp.MustCompile(`^(connector|tool|agent|version|principal|group|budget|price|policy)\.`)
)

// Decode strictly decodes a desired document: an unknown field (such as a
// secret someone tried to put in a bundle) is refused, not dropped.
func Decode(raw json.RawMessage) (Document, error) {
	var d Document
	if len(bytes.TrimSpace(raw)) == 0 {
		return d, errors.New("desired: required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("desired: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return d, errors.New("desired: trailing data after the document")
	}
	return d, nil
}

// Validate reports every structural problem of d as a blocking finding. The
// registry triggers check contract fields again when a step runs.
func Validate(d Document, raw json.RawMessage) []Finding {
	var out []Finding
	bad := func(addr, format string, args ...any) {
		out = append(out, Finding{Address: addr, Kind: KindInvalid, Detail: fmt.Sprintf(format, args...)})
	}
	if bytes.Contains(raw, []byte("${")) {
		out = append(out, Finding{Address: "bundle", Kind: KindUnresolvedVariable,
			Detail: "a ${...} reference was not resolved: resolve variables with eacpctl before planning"})
	}
	for _, name := range sortedKeys(d.Connectors) {
		c, addr := d.Connectors[name], "connector."+name
		if !slugRE.MatchString(name) {
			bad(addr, "connector names match %s", slugRE)
		}
		if c.Protocol != "http" && !discovered(c.Protocol) {
			bad(addr, "protocol is http, mcp or a2a")
		}
		if !endpointRE.MatchString(c.Endpoint) || len(c.Endpoint) > 2048 {
			bad(addr, "endpoint is an http(s) URL")
		}
		if !secretRE.MatchString(c.SecretRef) {
			bad(addr, "secret_ref names a secret the worker holds; it is never a secret value")
		}
		for _, tn := range sortedKeys(c.Tools) {
			if !toolNameRE.MatchString(tn) {
				bad("tool."+name+"."+tn, "tool names match %s", toolNameRE)
			}
			if ct := c.Tools[tn].Contract; ct != nil && ct.DefinitionID != nil {
				bad("contract."+name+"."+tn, "definition_id is pinned by the planner, never declared")
			}
		}
	}
	for _, name := range sortedKeys(d.Agents) {
		a, addr, vaddr := d.Agents[name], "agent."+name, "version."+name
		if !slugRE.MatchString(name) {
			bad(addr, "agent names match %s", slugRE)
		}
		if strings.TrimSpace(a.DisplayName) == "" {
			bad(addr, "display_name is required")
		}
		if !slices.Contains([]string{"development", "staging", "production"}, a.Environment) {
			bad(addr, "environment is development, staging or production")
		}
		if !slices.Contains([]string{"low", "medium", "high", "critical"}, a.RiskClass) {
			bad(addr, "risk_class is low, medium, high or critical")
		}
		if (a.Owner.Principal == "") == (a.Owner.Group == "") {
			bad(addr, "owner names exactly one principal or group")
		}
		if strings.TrimSpace(a.Version.Runtime) == "" || strings.TrimSpace(a.Version.CodeRef) == "" {
			bad(vaddr, "version needs runtime and code_ref")
		}
		if a.State != "" && a.State != string(registry.StateActive) {
			bad(vaddr, "state is ACTIVE or omitted")
		}
		if a.State == string(registry.StateActive) && len(a.Allowlist) == 0 {
			bad(vaddr, "an ACTIVE version needs an allowlist")
		}
		seen := map[string]bool{}
		for _, ref := range a.Allowlist {
			conn, tool, ok := strings.Cut(ref, ".")
			if !ok || conn == "" || tool == "" || strings.Contains(tool, ".") {
				bad("allowlist."+name, "%q is not connector.tool", ref)
			}
			if seen[ref] {
				bad("allowlist."+name, "%q is listed twice", ref)
			}
			seen[ref] = true
		}
	}
	out = append(out, validateGovernance(d)...)
	seen := map[string]bool{}
	for _, im := range d.Imports {
		addr := "import." + im.To
		switch {
		case !importRE.MatchString(im.To):
			bad(addr, "imports adopt a connector, tool, agent, version, principal, group, budget, price or the policy")
		case !declared(d, im.To):
			bad(addr, "%s is not declared in this bundle", im.To)
		case im.ID == uuid.Nil:
			bad(addr, "id is required")
		case seen[im.To]:
			bad(addr, "%s is imported twice", im.To)
		}
		seen[im.To] = true
	}
	return out
}

// declared reports whether the document declares addr.
func declared(d Document, addr string) bool {
	kind, name, _ := strings.Cut(addr, ".")
	switch kind {
	case "connector":
		_, ok := d.Connectors[name]
		return ok
	case "tool", "contract":
		conn, tool, _ := strings.Cut(name, ".")
		_, ok := d.Connectors[conn].Tools[tool]
		return ok
	case "agent", "version", "allowlist":
		_, ok := d.Agents[name]
		return ok
	}
	return declaredGovernance(d, kind, name)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// discovered reports a protocol whose tools the scanner discovers (MCP,
// ADR-023; A2A, ADR-030).
func discovered(protocol string) bool { return protocol == "mcp" || protocol == "a2a" }
