package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
)

const bundleUsage = `usage:
  eacpctl bundle validate [-C DIR] [-t TARGET] [--var NAME=VALUE]...
  eacpctl bundle plan     [-C DIR] [-t TARGET] [--var NAME=VALUE]... [--prune] [--dry-run]
  eacpctl bundle deploy   [-C DIR] [-t TARGET] [--var NAME=VALUE]... [--prune]
  eacpctl bundle approve|status <change-set-id> [-C DIR] [-t TARGET]
  eacpctl bundle reject <change-set-id> --reason TEXT [-C DIR] [-t TARGET]
  eacpctl bundle list|drift [-C DIR] [-t TARGET]`

// bundleConfig is eacp.yml. Files under resources/*.yml hold only a
// resources: block, merged into it.
type bundleConfig struct {
	Bundle struct {
		Name string `yaml:"name"`
	} `yaml:"bundle"`
	Targets   map[string]bundleTarget   `yaml:"targets"`
	Variables map[string]bundleVariable `yaml:"variables"`
	Resources bundleResources           `yaml:"resources"`
	Import    []bundleImport            `yaml:"import"`
	Prune     bool                      `yaml:"prune"`
}

// bundleTarget is an environment: its API, the tenant its key must belong
// to, and variable values.
type bundleTarget struct {
	API       string            `yaml:"api"`
	Tenant    string            `yaml:"tenant"`
	Default   bool              `yaml:"default"`
	Variables map[string]string `yaml:"variables"`
}

type bundleVariable struct {
	Default     *string `yaml:"default"`
	Description string  `yaml:"description"`
}

type bundleResources struct {
	Principals map[string]any `yaml:"principals"`
	Groups     map[string]any `yaml:"groups"`
	Connectors map[string]any `yaml:"connectors"`
	Agents     map[string]any `yaml:"agents"`
	Budgets    map[string]any `yaml:"budgets"`
	Prices     map[string]any `yaml:"prices"`
	Policy     *bundlePolicy  `yaml:"policy"`
}

// bundlePolicy names the tenant policy's JSON file, relative to the bundle
// directory.
type bundlePolicy struct {
	File string `yaml:"file"`
}

type section struct {
	key string
	m   *map[string]any
}

// sections are the mergeable resources: blocks, by their document key.
func (r *bundleResources) sections() []section {
	return []section{{"principals", &r.Principals}, {"groups", &r.Groups}, {"connectors", &r.Connectors},
		{"agents", &r.Agents}, {"budgets", &r.Budgets}, {"prices", &r.Prices}}
}

type bundleImport struct {
	To string `yaml:"to"`
	ID string `yaml:"id"`
}

type loadedBundle struct {
	name       string
	targetName string
	target     bundleTarget
	prune      bool
	desired    map[string]any
}

var varRE = regexp.MustCompile(`\$\{var\.([A-Za-z_][A-Za-z0-9_]*)\}`)

// errNoValue marks a variable without a value: fatal for validate, plan and
// deploy, which send the document, but not for list and drift, which need
// only the bundle's name.
var errNoValue = errors.New("variable has no value")

func decodeYAMLFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// loadBundle reads dir/eacp.yml and dir/resources/*.yml, picks the target
// (the named one, else the default one, else the only one), and resolves
// ${var.NAME} from --var, then the target, then the variable's default.
func loadBundle(dir, targetName string, vars map[string]string) (loadedBundle, error) {
	var cfg bundleConfig
	if err := decodeYAMLFile(filepath.Join(dir, "eacp.yml"), &cfg); err != nil {
		return loadedBundle{}, err
	}
	if cfg.Bundle.Name == "" {
		return loadedBundle{}, errors.New("eacp.yml: bundle.name is required")
	}
	var res bundleResources
	var policy *bundlePolicy
	merge := func(src *bundleResources, file string) error {
		dst := res.sections()
		for i, s := range src.sections() {
			if *dst[i].m == nil {
				*dst[i].m = map[string]any{}
			}
			for k, v := range *s.m {
				if _, dup := (*dst[i].m)[k]; dup {
					return fmt.Errorf("%s: %s %q is declared twice", file, strings.TrimSuffix(s.key, "s"), k)
				}
				(*dst[i].m)[k] = v
			}
		}
		if src.Policy != nil {
			if policy != nil {
				return fmt.Errorf("%s: the policy is declared twice", file)
			}
			policy = src.Policy
		}
		return nil
	}
	if err := merge(&cfg.Resources, "eacp.yml"); err != nil {
		return loadedBundle{}, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "resources", "*.yml"))
	if err != nil {
		return loadedBundle{}, err
	}
	slices.Sort(files)
	for _, f := range files {
		var rf struct {
			Resources bundleResources `yaml:"resources"`
		}
		if err := decodeYAMLFile(f, &rf); err != nil {
			return loadedBundle{}, err
		}
		if err := merge(&rf.Resources, f); err != nil {
			return loadedBundle{}, err
		}
	}

	b := loadedBundle{name: cfg.Bundle.Name, prune: cfg.Prune}
	switch {
	case targetName != "":
		t, ok := cfg.Targets[targetName]
		if !ok {
			return b, fmt.Errorf("eacp.yml: no target %q", targetName)
		}
		b.target, b.targetName = t, targetName
	default:
		for name, t := range cfg.Targets {
			if t.Default || len(cfg.Targets) == 1 {
				if b.targetName != "" {
					return b, errors.New("eacp.yml: more than one default target: pass -t")
				}
				b.target, b.targetName = t, name
			}
		}
	}

	values := map[string]string{}
	for name, v := range cfg.Variables {
		if v.Default != nil {
			values[name] = *v.Default
		}
	}
	for _, layer := range []map[string]string{b.target.Variables, vars} {
		for k, v := range layer {
			if _, declared := cfg.Variables[k]; !declared {
				return b, fmt.Errorf("variable %q is not declared in eacp.yml", k)
			}
			values[k] = v
		}
	}

	desired := map[string]any{}
	for _, s := range res.sections() {
		if len(*s.m) > 0 {
			desired[s.key] = *s.m
		}
	}
	if policy != nil {
		content, err := readPolicy(dir, policy.File)
		if err != nil {
			return b, err
		}
		desired["policy"] = map[string]any{"content": content}
	}
	if len(cfg.Import) > 0 {
		imports := make([]any, 0, len(cfg.Import))
		for _, im := range cfg.Import {
			imports = append(imports, map[string]any{"to": im.To, "id": im.ID})
		}
		desired["imports"] = imports
	}
	resolved, err := substitute(desired, values)
	if err != nil {
		return b, err
	}
	b.desired = resolved.(map[string]any)
	return b, nil
}

// readPolicy reads the policy file as JSON with its numbers intact. It is
// opened through an os.Root, so neither "..", an absolute path nor a symlink
// or junction reaches outside the bundle directory; it is a regular file of
// at most 1 MiB.
func readPolicy(dir, file string) (any, error) {
	if file == "" || filepath.IsAbs(file) {
		return nil, fmt.Errorf("policy.file %q: give a path relative to the bundle directory", file)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("policy.file: %v", err)
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(file))
	if err != nil {
		return nil, fmt.Errorf("policy.file %q: stays inside the bundle directory: %v", file, err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("policy.file %q: is not a regular file", file)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, fmt.Errorf("policy.file: %v", err)
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("policy.file %q: over 1 MiB", file)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("policy.file %q: %v", file, err)
	}
	return v, nil
}

func substitute(v any, values map[string]string) (any, error) {
	switch x := v.(type) {
	case string:
		var missing string
		out := varRE.ReplaceAllStringFunc(x, func(m string) string {
			name := varRE.FindStringSubmatch(m)[1]
			val, ok := values[name]
			if !ok {
				missing = name
				return m
			}
			return val
		})
		if missing != "" {
			return nil, fmt.Errorf("%w: %q: give it a default, set it in the target, or pass --var", errNoValue,
				missing)
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			r, err := substitute(e, values)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := substitute(e, values)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// checkAPI refuses a target whose api disagrees with EACP_API_URL. A bundle
// comes from a repository, so its api only asserts where the target lives:
// it never redirects the API key, and EACP_API_URL stays required.
func checkAPI(getenv func(string) string, b loadedBundle) error {
	if b.target.API == "" {
		return nil
	}
	set := getenv("EACP_API_URL")
	if strings.TrimRight(set, "/") != strings.TrimRight(b.target.API, "/") {
		return fmt.Errorf("target %s is at %s, but EACP_API_URL is %q: set EACP_API_URL to use this target",
			b.targetName, b.target.API, set)
	}
	return nil
}

// checkTenant refuses to plan against a tenant other than the target's.
func checkTenant(ctx context.Context, getenv func(string) string, b loadedBundle) error {
	if b.target.Tenant == "" {
		return nil
	}
	raw, err := request(ctx, getenv, "GET", "/v1/me", nil)
	if err != nil {
		return err
	}
	var me struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		return err
	}
	if me.TenantID != b.target.Tenant {
		return fmt.Errorf("target %s is tenant %s, but EACP_API_KEY belongs to tenant %s", b.targetName,
			b.target.Tenant, me.TenantID)
	}
	return nil
}

func runBundle(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(bundleUsage)
	}
	cmd, rest := args[0], args[1:]
	var id string
	switch cmd {
	case "approve", "status", "reject":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(bundleUsage)
		}
		if _, err := uuid.Parse(rest[0]); err != nil {
			return errors.New(bundleUsage)
		}
		id, rest = rest[0], rest[1:]
	case "validate", "plan", "deploy", "list", "drift":
	default:
		return errors.New(bundleUsage)
	}
	fs := newFlags("bundle " + cmd)
	dir := fs.String("C", ".", "bundle directory")
	target := fs.String("t", "", "target")
	prune := fs.Bool("prune", false, "retire or revoke what the bundle no longer declares")
	dryRun := fs.Bool("dry-run", false, "plan without recording a change set")
	reason := fs.String("reason", "", "why the change set is rejected")
	vars := map[string]string{}
	fs.Func("var", "NAME=VALUE", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return errors.New("--var takes NAME=VALUE")
		}
		vars[k] = val
		return nil
	})
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || (cmd == "reject" && *reason == "") {
		return errors.New(bundleUsage)
	}

	// approve, status and reject need only the API and never read a bundle.
	// list and drift need only its name and target.
	var b loadedBundle
	needsDocument := cmd == "validate" || cmd == "plan" || cmd == "deploy"
	if id == "" {
		var err error
		b, err = loadBundle(*dir, *target, vars)
		if err != nil && (needsDocument || !errors.Is(err, errNoValue)) {
			return err
		}
		if cmd != "validate" {
			if err := checkAPI(getenv, b); err != nil {
				return err
			}
		}
	}
	env := getenv

	switch cmd {
	case "validate":
		raw, err := json.Marshal(map[string]any{"bundle": b.name, "target": b.targetName, "desired": b.desired})
		if err != nil {
			return err
		}
		return printJSON(out, raw)
	case "approve":
		return call(ctx, env, out, "POST", "/v1/change-sets/"+id+"/approve", nil)
	case "reject":
		return call(ctx, env, out, "POST", "/v1/change-sets/"+id+"/reject", map[string]any{"reason": *reason})
	case "status":
		return call(ctx, env, out, "GET", "/v1/change-sets/"+id, nil)
	case "list":
		return call(ctx, env, out, "GET", "/v1/change-sets?"+url.Values{"bundle": {b.name}}.Encode(), nil)
	case "drift":
		return call(ctx, env, out, "GET", "/v1/bundles/"+url.PathEscape(b.name)+"/drift", nil)
	}

	if err := checkTenant(ctx, env, b); err != nil {
		return err
	}
	body := map[string]any{"bundle": b.name, "desired": b.desired, "prune": *prune || b.prune}
	if cmd == "plan" && *dryRun {
		body["dry_run"] = true
	}
	raw, err := request(ctx, env, "POST", "/v1/change-sets", body)
	if err != nil {
		return err
	}
	if err := printJSON(out, raw); err != nil || cmd == "plan" {
		return err
	}
	var planned struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &planned); err != nil {
		return err
	}
	if planned.ID == "" {
		_, err := fmt.Fprintln(out, "no changes: the registry matches the bundle")
		return err
	}
	return call(ctx, env, out, "POST", "/v1/change-sets/"+planned.ID+"/submit", nil)
}
