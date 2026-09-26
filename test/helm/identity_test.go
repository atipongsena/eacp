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
	for _, c := range list(spec, "containers") {
		for _, m := range list(c, "volumeMounts") {
			if get(m, "mountPath") == "/run/secrets/eacp-identity" {
				return true
			}
		}
	}
	return false
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
		if len(sources) != 1 {
			t.Fatalf("the worker projects %d tokens, want 1", len(sources))
		}
		tok := sources[0]
		if get(tok, "audience") != "fakeerp" || get(tok, "expirationSeconds") != 600 || get(tok, "path") != "token" {
			t.Errorf("the worker's projected token = %v", tok)
		}
		readOnly := false
		for _, c := range list(spec, "containers") {
			for _, m := range list(c, "volumeMounts") {
				if get(m, "mountPath") == "/run/secrets/eacp-identity" && get(m, "readOnly") == true {
					readOnly = true
				}
			}
		}
		if !readOnly {
			t.Error("the identity token is not mounted read-only at /run/secrets/eacp-identity")
		}
	}
}

func TestWorkloadIdentityOffProjectsNothing(t *testing.T) {
	objs := render(t, "--set", "worker.workloadIdentity.enabled=false")
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
