package helm

import (
	"fmt"
	"strings"
	"testing"
)

var workloads = []struct{ kind, name, component string }{
	{"Deployment", "eacp-api", "api"}, {"Deployment", "eacp-worker", "worker"},
	{"Deployment", "eacp-pdp", "pdp"}, {"Job", "eacp-migrate", "migrate"},
}

func TestEveryPodIsHardened(t *testing.T) {
	objs := render(t)
	for _, w := range workloads {
		o := find(t, objs, w.kind, w.name)
		spec := podSpec(o)
		if get(o, "spec", "template", "metadata", "labels", "app.kubernetes.io/component") != w.component {
			t.Errorf("%s: component label missing", w.name)
		}
		if spec["automountServiceAccountToken"] != false || spec["enableServiceLinks"] != false {
			t.Errorf("%s: service account token or service links not disabled", w.name)
		}
		psc := get(spec, "securityContext")
		if get(psc, "runAsNonRoot") != true || get(psc, "runAsUser") != 65532 || get(psc, "runAsGroup") != 65532 ||
			get(psc, "seccompProfile", "type") != "RuntimeDefault" {
			t.Errorf("%s: pod securityContext = %v", w.name, psc)
		}
		tmp := false
		for _, v := range list(spec, "volumes") {
			if get(v, "name") == "tmp" && get(v, "emptyDir") != nil {
				tmp = true
			}
		}
		if !tmp {
			t.Errorf("%s: no /tmp emptyDir", w.name)
		}
		for _, c := range list(spec, "containers") {
			sc := get(c, "securityContext")
			caps := fmt.Sprint(get(sc, "capabilities", "drop"))
			if get(sc, "allowPrivilegeEscalation") != false || get(sc, "readOnlyRootFilesystem") != true || caps != "[ALL]" {
				t.Errorf("%s/%v: container securityContext = %v", w.name, get(c, "name"), sc)
			}
			if get(c, "resources", "limits", "memory") == nil || get(c, "resources", "requests", "cpu") == nil {
				t.Errorf("%s: resources not set", w.name)
			}
			if w.kind == "Deployment" && (get(c, "readinessProbe") == nil || get(c, "livenessProbe") == nil ||
				get(c, "startupProbe") == nil) {
				t.Errorf("%s: probes missing", w.name)
			}
		}
	}
	if len(all(objs, "Role"))+len(all(objs, "ClusterRole"))+len(all(objs, "RoleBinding"))+len(all(objs, "ClusterRoleBinding")) != 0 {
		t.Error("the chart renders RBAC objects")
	}
	sa := find(t, objs, "ServiceAccount", "eacp")
	if sa["automountServiceAccountToken"] != false {
		t.Error("service account mounts its token")
	}
}

// secretRefs lists every Secret name a pod spec references, with the keys
// it projects ("" for a whole-secret reference).
func secretRefs(spec map[string]any) map[string][]string {
	refs := map[string][]string{}
	for _, v := range list(spec, "volumes") {
		if n, ok := get(v, "secret", "secretName").(string); ok {
			for _, it := range list(v, "secret", "items") {
				refs[n] = append(refs[n], fmt.Sprint(get(it, "key")))
			}
			if len(list(v, "secret", "items")) == 0 {
				refs[n] = append(refs[n], "")
			}
		}
		for _, src := range list(v, "projected", "sources") {
			if n, ok := get(src, "secret", "name").(string); ok {
				for _, it := range list(src, "secret", "items") {
					refs[n] = append(refs[n], fmt.Sprint(get(it, "key")))
				}
			}
		}
	}
	for _, c := range list(spec, "containers") {
		for _, e := range list(c, "env") {
			if n, ok := get(e, "valueFrom", "secretKeyRef", "name").(string); ok {
				refs[n] = append(refs[n], fmt.Sprint(get(e, "valueFrom", "secretKeyRef", "key")))
			}
		}
		if len(list(c, "envFrom")) > 0 {
			refs["<envFrom>"] = append(refs["<envFrom>"], "")
		}
	}
	return refs
}

func TestSecretsReachOnlyTheirPods(t *testing.T) {
	objs := render(t)
	want := map[string]map[string]string{ // component -> secret -> keys
		"api":     {"eacp-db-app": "url", "eacp-pdp-tls": "ca.pem client-key.pem client.pem", "eacp-nats-relay": "url"},
		"worker":  {"eacp-db-app": "url", "eacp-connector-secrets": "connector-secrets.json", "eacp-nats-worker": "url"},
		"pdp":     {"eacp-pdp-tls": "ca.pem server-key.pem server.pem"},
		"migrate": {"eacp-db-owner": "url"},
	}
	for _, w := range workloads {
		refs := secretRefs(podSpec(find(t, objs, w.kind, w.name)))
		got := map[string]string{}
		for n, keys := range refs {
			got[n] = strings.Join(sortedUnique(keys), " ")
		}
		if fmt.Sprint(got) != fmt.Sprint(want[w.component]) {
			t.Errorf("%s references secrets %v, want %v", w.name, got, want[w.component])
		}
	}
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestNoSecretValueIsRendered(t *testing.T) {
	out, err := renderErr(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"kind: Secret", "postgres://", "nats://", "dev-only-", "BEGIN "} {
		if strings.Contains(out, bad) {
			t.Errorf("rendered manifests contain %q", bad)
		}
	}
}

func TestMigrationsRunAsAHookWithTheOwnerRole(t *testing.T) {
	job := find(t, render(t), "Job", "eacp-migrate")
	ann := get(job, "metadata", "annotations")
	if get(ann, "helm.sh/hook") != "pre-install,pre-upgrade" ||
		get(ann, "helm.sh/hook-delete-policy") != "before-hook-creation,hook-succeeded" {
		t.Fatalf("migrate hook annotations = %v", ann)
	}
	c := list(podSpec(job), "containers")[0]
	if fmt.Sprint(get(c, "args")) != "[migrate up]" || get(c, "command") == nil {
		t.Fatalf("migrate container = %v %v", get(c, "command"), get(c, "args"))
	}
	if get(job, "spec", "backoffLimit") == nil || podSpec(job)["restartPolicy"] != "Never" {
		t.Fatal("migrate Job must bound retries and never restart in place")
	}
}

func TestWorkerIdentityComesFromThePodName(t *testing.T) {
	objs := render(t)
	for name, key := range map[string]string{"eacp-worker": "EACP_WORKER_ID", "eacp-pdp": "AGT_PDP_INSTANCE_ID"} {
		found := false
		for _, e := range list(list(podSpec(find(t, objs, "Deployment", name)), "containers")[0], "env") {
			if get(e, "name") == key && get(e, "valueFrom", "fieldRef", "fieldPath") == "metadata.name" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %s is not the pod name", name, key)
		}
	}
}

func TestDangerousValuesAreRefused(t *testing.T) {
	for set, msg := range map[string]string{
		"api.env.EACP_CONNECTOR_SECRETS_FILE=/x":          "connector secrets",
		"pdp.env.EACP_CONNECTOR_SECRETS_FILE=/x":          "connector secrets",
		"api.env.EACP_DATABASE_URL=postgres://a:b@c/d":    "EACP_DATABASE_URL",
		"worker.env.EACP_X_URL=nats://u:p@h:4222":         "password",
		"database.appSecret=":                             "database.appSecret",
		"database.ownerSecret=":                           "database.ownerSecret",
		"pdp.tlsSecret=":                                  "pdp.tlsSecret",
		"worker.connectorSecrets=":                        "worker.connectorSecrets",
		"api.replicas=0":                                  "replicas",
		"governance.provider=local":                       "governance.allowLocal",
		"governance.provider=other":                       "governance.provider",
		"database.peers=null":                             "database.peers",
		"nats.peers=null":                                 "nats.peers",
		"worker.workloadIdentity.audience=":               "workloadIdentity.audience",
		"worker.workloadIdentity.audience=a b":            "workloadIdentity.audience",
		"worker.workloadIdentity.expirationSeconds=599":   "workloadIdentity.expirationSeconds",
		"worker.workloadIdentity.expirationSeconds=86401": "workloadIdentity.expirationSeconds",
		"worker.workloadIdentity.expirationSeconds=1.5":   "workloadIdentity.expirationSeconds",
		"worker.vaultIdentity.audience=":                  "vaultIdentity.audience",
		"worker.vaultIdentity.audience=a b":               "vaultIdentity.audience",
		"worker.vaultIdentity.expirationSeconds=599":      "vaultIdentity.expirationSeconds",
		"worker.vaultIdentity.expirationSeconds=86401":    "vaultIdentity.expirationSeconds",
		"worker.vaultIdentity.expirationSeconds=1.5":      "vaultIdentity.expirationSeconds",
	} {
		out, err := renderErr(t, "--set", set)
		if err == nil || !strings.Contains(out, msg) {
			t.Errorf("--set %s: err = %v, output lacks %q:\n%s", set, err, msg, firstLines(out))
		}
	}
}

func firstLines(s string) string {
	lines := strings.SplitN(s, "\n", 4)
	return strings.Join(lines[:min(3, len(lines))], "\n")
}
