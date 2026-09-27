package helm

import "testing"

// tokenSources lists the serviceAccountToken projections of a pod spec.
func tokenSources(spec map[string]any) []any {
	var out []any
	for _, v := range list(spec, "volumes") {
		for _, s := range list(v, "projected", "sources") {
			if tok := get(s, "serviceAccountToken"); tok != nil {
				out = append(out, tok)
			}
		}
	}
	return out
}

func identityMounted(spec map[string]any) bool {
	return mountedAt(spec, "/run/secrets/eacp-identity") || mountedAt(spec, "/run/secrets/eacp-vault-identity")
}

// mountedAt reports whether a container mounts a volume at path, and
// whether read-only.
func mountedAt(spec map[string]any, path string) bool {
	for _, c := range list(spec, "containers") {
		for _, m := range list(c, "volumeMounts") {
			if get(m, "mountPath") == path {
				return true
			}
		}
	}
	return false
}

func mountedReadOnly(spec map[string]any, path string) bool {
	for _, c := range list(spec, "containers") {
		for _, m := range list(c, "volumeMounts") {
			if get(m, "mountPath") == path && get(m, "readOnly") == true {
				return true
			}
		}
	}
	return false
}

// tokenByAudience returns the worker's projected token for audience.
func tokenByAudience(spec map[string]any, audience string) any {
	for _, tok := range tokenSources(spec) {
		if get(tok, "audience") == audience {
			return tok
		}
	}
	return nil
}

// ADR-019 Rev 1.1: a federated subject names only the worker.
func TestTheWorkerHasItsOwnServiceAccount(t *testing.T) {
	objs := render(t)
	sa := find(t, objs, "ServiceAccount", "eacp-worker")
	if sa["automountServiceAccountToken"] != false {
		t.Error("the worker's service account mounts its token")
	}
	for _, w := range workloads {
		if w.kind != "Deployment" {
			continue
		}
		want := "eacp"
		if w.component == "worker" {
			want = "eacp-worker"
		}
		if got := podSpec(find(t, objs, w.kind, w.name))["serviceAccountName"]; got != want {
			t.Errorf("%s runs as service account %v, want %s", w.name, got, want)
		}
	}
}

func TestOnlyTheWorkerProjectsAnIdentityToken(t *testing.T) {
	objs := render(t) // the e2e values enable workload identity (audience fakeerp, 600 s)
	for _, w := range workloads {
		spec := podSpec(find(t, objs, w.kind, w.name))
		sources := tokenSources(spec)
		if w.component != "worker" {
			if len(sources) != 0 || identityMounted(spec) {
				t.Errorf("%s projects a service-account token", w.name)
			}
			continue
		}
		if len(sources) != 2 { // the e2e values also enable the Vault identity
			t.Fatalf("the worker projects %d tokens, want 2", len(sources))
		}
		tok := tokenByAudience(spec, "fakeerp")
		if tok == nil || get(tok, "expirationSeconds") != 600 || get(tok, "path") != "token" {
			t.Errorf("the worker's projected token = %v", tok)
		}
		if !mountedReadOnly(spec, "/run/secrets/eacp-identity") {
			t.Error("the identity token is not mounted read-only at /run/secrets/eacp-identity")
		}
	}
}

func TestWorkloadIdentityOffProjectsNothing(t *testing.T) {
	objs := render(t, "--set", "worker.workloadIdentity.enabled=false", "--set", "worker.vaultIdentity.enabled=false")
	for _, w := range workloads {
		spec := podSpec(find(t, objs, w.kind, w.name))
		if len(tokenSources(spec)) != 0 || identityMounted(spec) {
			t.Errorf("%s projects a token with workload identity off", w.name)
		}
	}
	if podSpec(find(t, objs, "Deployment", "eacp-worker"))["serviceAccountName"] != "eacp-worker" {
		t.Error("the worker lost its own service account")
	}
}

// ADR-019 Rev 1.3: the worker's Vault login token has its own audience, so
// neither Vault nor the ERP's IdP can replay a token meant for the other.
func TestTheVaultIdentityIsTheWorkersOwn(t *testing.T) {
	for name, args := range map[string][]string{
		"with workload identity":    nil,
		"without workload identity": {"--set", "worker.workloadIdentity.enabled=false"},
	} {
		objs := render(t, args...)
		for _, w := range workloads {
			spec := podSpec(find(t, objs, w.kind, w.name))
			if w.component != "worker" {
				if mountedAt(spec, "/run/secrets/eacp-vault-identity") {
					t.Errorf("%s: %s mounts the Vault identity", name, w.name)
				}
				continue
			}
			tok := tokenByAudience(spec, "vault")
			if tok == nil || get(tok, "expirationSeconds") != 3600 || get(tok, "path") != "token" {
				t.Errorf("%s: the worker's Vault token = %v", name, tok)
			}
			if !mountedReadOnly(spec, "/run/secrets/eacp-vault-identity") {
				t.Errorf("%s: the Vault identity is not mounted read-only at /run/secrets/eacp-vault-identity", name)
			}
		}
	}
	off := podSpec(find(t, render(t, "--set", "worker.vaultIdentity.enabled=false"), "Deployment", "eacp-worker"))
	if tokenByAudience(off, "vault") != nil || mountedAt(off, "/run/secrets/eacp-vault-identity") {
		t.Error("the Vault identity is projected while disabled")
	}
}

// spiffeVolume returns the pod's SPIFFE Workload API volume, or nil.
func spiffeVolume(spec map[string]any) any {
	for _, v := range list(spec, "volumes") {
		if get(v, "name") == "spiffe-workload-api" {
			return v
		}
	}
	return nil
}

// ADR-019 Rev 1.4: only the worker reaches the SPIRE agent, through the
// SPIFFE CSI driver's read-only socket volume (allowed under the restricted
// Pod Security Standard; a hostPath socket would not be).
func TestTheSPIFFESocketIsTheWorkersOwn(t *testing.T) {
	if s, _ := get(defaults(t), "worker", "spiffe", "enabled").(bool); s {
		t.Fatal("worker.spiffe is enabled by default")
	}
	for _, w := range workloads {
		spec := podSpec(find(t, render(t, "--set", "worker.spiffe.enabled=false"), w.kind, w.name))
		if spiffeVolume(spec) != nil || mountedAt(spec, "/spiffe-workload-api") {
			t.Errorf("%s mounts the SPIFFE socket while it is disabled", w.name)
		}
	}
	for driver, args := range map[string][]string{
		"csi.spiffe.io":    {"--set", "worker.spiffe.enabled=true"},
		"csi.example.test": {"--set", "worker.spiffe.enabled=true", "--set", "worker.spiffe.csiDriver=csi.example.test"},
	} {
		objs := render(t, args...)
		for _, w := range workloads {
			spec := podSpec(find(t, objs, w.kind, w.name))
			vol := spiffeVolume(spec)
			if w.component != "worker" {
				if vol != nil || mountedAt(spec, "/spiffe-workload-api") {
					t.Errorf("%s mounts the SPIFFE socket", w.name)
				}
				continue
			}
			if get(vol, "csi", "driver") != driver || get(vol, "csi", "readOnly") != true || len(get(vol, "csi").(map[string]any)) != 2 {
				t.Errorf("the worker's SPIFFE volume = %v, want a read-only csi volume of %s", vol, driver)
			}
			if !mountedReadOnly(spec, "/spiffe-workload-api") {
				t.Error("the SPIFFE socket is not mounted read-only at /spiffe-workload-api")
			}
		}
	}
}
