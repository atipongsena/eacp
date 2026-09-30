package bundle

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/atipongsena/eacp/internal/governance"
)

// Principal is a declared principal. Its name, kind and subject are
// immutable; Roles is the full set of approved roles it should hold.
type Principal struct {
	Kind        string   `json:"kind"`
	Subject     string   `json:"subject,omitempty"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles,omitempty"`
}

// Group is a declared group. ScheduleWeight is set only when the group is
// created; Members is the full set of principals in it.
type Group struct {
	DisplayName    string   `json:"display_name"`
	ScheduleWeight int      `json:"schedule_weight,omitempty"`
	Members        []string `json:"members,omitempty"`
}

// Policy is the tenant's governance policy, a local policy bundle.
type Policy struct {
	Content json.RawMessage `json:"content"`
}

// Budget is a declared budget account. Unit, Parent and Agent are
// immutable; an omitted SoftLimit leaves the current one alone.
type Budget struct {
	Unit      string       `json:"unit"`
	Parent    string       `json:"parent,omitempty"`
	Agent     string       `json:"agent,omitempty"`
	HardLimit json.Number  `json:"hard_limit"`
	SoftLimit *json.Number `json:"soft_limit,omitempty"`
}

// Price is the price that should be in effect for a provider and model. A
// different one is added, effective when it is applied; a bundle never
// removes or backdates a price.
type Price struct {
	Provider      string       `json:"provider"`
	Model         string       `json:"model"`
	Unit          string       `json:"unit"`
	InputPerMTok  json.Number  `json:"input_per_mtok"`
	CachedPerMTok *json.Number `json:"cached_input_per_mtok,omitempty"`
	OutputPerMTok json.Number  `json:"output_per_mtok"`
}

// PolicyAddress is the address of the tenant policy.
const PolicyAddress = "policy.tenant"

// maxPolicy keeps a policy step within a step's 64 KiB payload.
const maxPolicy = 60000

const amountRule = "is a non-negative amount below 10^15 with at most 6 decimals"

var (
	unitRE     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,15}$`)
	providerRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	roleNames  = []string{"admin", "registry_editor", "registry_approver", "operator", "approver", "auditor",
		"studio_author", "studio_runtime"}
	million   = big.NewRat(1_000_000, 1)
	amountCap = new(big.Rat).SetInt64(1_000_000_000_000_000)
)

// amount returns n in the canonical form of a numeric(21,6) column, if the
// column stores it exactly.
func amount(n json.Number) (string, bool) {
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 || r.Cmp(amountCap) >= 0 || !new(big.Rat).Mul(r, million).IsInt() {
		return "", false
	}
	s := strings.TrimRight(strings.TrimRight(r.FloatString(6), "0"), ".")
	if s == "" {
		s = "0"
	}
	return s, true
}

// cmpAmount compares two amounts by value.
func cmpAmount(a, b string) int {
	x, okX := new(big.Rat).SetString(a)
	y, okY := new(big.Rat).SetString(b)
	if !okX || !okY {
		return strings.Compare(a, b)
	}
	return x.Cmp(y)
}

// validateGovernance reports the structural problems of the identity,
// policy, budget and price sections as blocking findings.
func validateGovernance(d Document) []Finding {
	var out []Finding
	bad := func(addr, format string, args ...any) {
		out = append(out, Finding{Address: addr, Kind: KindInvalid, Detail: fmt.Sprintf(format, args...)})
	}
	for _, name := range sortedKeys(d.Principals) {
		p, addr := d.Principals[name], "principal."+name
		if !slugRE.MatchString(name) {
			bad(addr, "principal names match %s", slugRE)
		}
		switch p.Kind {
		case "human":
			if s := strings.TrimSpace(p.Subject); len(s) < 3 || len(s) > 320 {
				bad(addr, "a human needs a subject (an IdP subject or email)")
			}
		case "service":
			if p.Subject != "" {
				bad(addr, "a service principal has no subject")
			}
		default:
			bad(addr, "kind is human or service")
		}
		if strings.TrimSpace(p.DisplayName) == "" {
			bad(addr, "display_name is required")
		}
		seen := map[string]bool{}
		for _, r := range p.Roles {
			switch {
			case !slices.Contains(roleNames, r):
				bad(addr, "unknown role %q", r)
			case p.Kind == "service" && r != "auditor" && r != "studio_runtime":
				bad(addr, "a service principal may hold only auditor or studio_runtime (ADR-003 §1, ADR-033)")
			case p.Kind == "human" && r == "studio_runtime":
				bad(addr, "studio_runtime is held only by a service principal (ADR-033)")
			case r == "studio_runtime" && len(p.Roles) > 1:
				bad(addr, "studio_runtime is held alone (ADR-033)")
			case seen[r]:
				bad(addr, "role %q is listed twice", r)
			}
			seen[r] = true
		}
	}
	for _, name := range sortedKeys(d.Groups) {
		g, addr := d.Groups[name], "group."+name
		if !slugRE.MatchString(name) {
			bad(addr, "group names match %s", slugRE)
		}
		if strings.TrimSpace(g.DisplayName) == "" {
			bad(addr, "display_name is required")
		}
		if g.ScheduleWeight != 0 && (g.ScheduleWeight < 1 || g.ScheduleWeight > 10) {
			bad(addr, "schedule_weight is 1 to 10")
		}
		seen := map[string]bool{}
		for _, m := range g.Members {
			switch {
			case !slugRE.MatchString(m):
				bad(addr, "member %q is not a principal name", m)
			case seen[m]:
				bad(addr, "%s is listed twice", m)
			}
			seen[m] = true
		}
	}
	if d.Policy != nil {
		if len(d.Policy.Content) > maxPolicy {
			bad(PolicyAddress, "the policy is over %d bytes", maxPolicy)
		} else if err := governance.ValidatePolicy(d.Policy.Content); err != nil {
			bad(PolicyAddress, "%v", err)
		}
	}
	for _, name := range sortedKeys(d.Budgets) {
		b, addr := d.Budgets[name], "budget."+name
		if !toolNameRE.MatchString(name) {
			bad(addr, "budget names match %s", toolNameRE)
		}
		if !unitRE.MatchString(b.Unit) {
			bad(addr, "unit matches %s", unitRE)
		}
		if _, ok := amount(b.HardLimit); !ok {
			bad(addr, "hard_limit %s", amountRule)
		}
		if b.SoftLimit != nil {
			if s, ok := amount(*b.SoftLimit); !ok || s == "0" {
				bad(addr, "soft_limit is a positive amount below 10^15 with at most 6 decimals")
			}
		}
		if b.Agent != "" && !slugRE.MatchString(b.Agent) {
			bad(addr, "agent %q is not an agent name", b.Agent)
		}
		if par, ok := d.Budgets[b.Parent]; b.Parent != "" && ok {
			if par.Unit != b.Unit {
				bad(addr, "parent %s is in %s, not %s", b.Parent, par.Unit, b.Unit)
			}
			if par.Agent != "" {
				bad(addr, "parent %s belongs to agent %s; an agent's account has no children", b.Parent, par.Agent)
			}
		}
		for cur, n := b.Parent, 0; cur != "" && n <= len(d.Budgets); cur, n = d.Budgets[cur].Parent, n+1 {
			if cur == name {
				bad(addr, "budget parents form a cycle")
				break
			}
		}
	}
	keys := map[string]string{}
	for _, name := range sortedKeys(d.Prices) {
		p, addr := d.Prices[name], "price."+name
		if !toolNameRE.MatchString(name) {
			bad(addr, "price names match %s", toolNameRE)
		}
		if !providerRE.MatchString(p.Provider) {
			bad(addr, "provider matches %s", providerRE)
		}
		if len(p.Model) == 0 || len(p.Model) > 256 || strings.ContainsFunc(p.Model, unicode.IsControl) {
			bad(addr, "model is 1 to 256 characters without control characters")
		}
		if !unitRE.MatchString(p.Unit) {
			bad(addr, "unit matches %s", unitRE)
		}
		for _, f := range []struct {
			field string
			n     *json.Number
		}{{"input_per_mtok", &p.InputPerMTok}, {"cached_input_per_mtok", p.CachedPerMTok},
			{"output_per_mtok", &p.OutputPerMTok}} {
			if f.n != nil {
				if _, ok := amount(*f.n); !ok {
					bad(addr, "%s %s", f.field, amountRule)
				}
			}
		}
		key := p.Provider + " " + p.Model
		if other, dup := keys[key]; dup {
			bad(addr, "price.%s already prices %s", other, key)
		}
		keys[key] = name
	}
	return out
}

// declaredGovernance reports whether d declares an identity, policy, budget
// or price address.
func declaredGovernance(d Document, kind, name string) bool {
	switch kind {
	case "principal":
		_, ok := d.Principals[name]
		return ok
	case "role":
		p, role, _ := strings.Cut(name, ".")
		return slices.Contains(d.Principals[p].Roles, role)
	case "group":
		_, ok := d.Groups[name]
		return ok
	case "member":
		g, p, _ := strings.Cut(name, ".")
		return slices.Contains(d.Groups[g].Members, p)
	case "policy":
		return name == "tenant" && d.Policy != nil
	case "budget":
		_, ok := d.Budgets[name]
		return ok
	case "price":
		_, ok := d.Prices[name]
		return ok
	}
	return false
}
