package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAPI accepts the principal keys in people on /v1/me and the agent keys
// in agents on /v1/agent/self, like the control plane does.
func fakeAPI(t *testing.T, people, agents []string) *httptest.Server {
	t.Helper()
	ok := func(keys []string, r *http.Request) bool {
		for _, k := range keys {
			if r.Header.Get("Authorization") == "Bearer "+k {
				return true
			}
		}
		return false
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/me" && ok(people, r), r.URL.Path == "/v1/agent/self" && ok(agents, r):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func envFile(t *testing.T, env map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := writeEnv(path, env); err != nil {
		t.Fatal(err)
	}
	return path
}

var fullEnv = map[string]string{"EACP_API": "x", "ADMIN_KEY": "admin", "OPERATOR_KEY": "otto", "AGENT_KEY": "agent"}

func setupFor(srv *httptest.Server) *setup {
	return &setup{api: srv.URL, http: srv.Client(), keys: map[string]string{}, env: map[string]string{}, ids: map[string]string{}}
}

func TestAlreadySetUpWhenEveryKeyWorks(t *testing.T) {
	srv := fakeAPI(t, []string{"admin", "otto"}, []string{"agent"})
	done, err := setupFor(srv).alreadySetUp(context.Background(), envFile(t, fullEnv))
	if err != nil || !done {
		t.Fatalf("done=%v err=%v, want a working .env to be reused", done, err)
	}
}

// People and agent keys expire before the admins' (or a stack is half
// reset): setup must not say "nothing to do" while the examples get 401.
func TestAlreadySetUpRefusesAnyKeyThatNoLongerWorks(t *testing.T) {
	for name, srv := range map[string]*httptest.Server{
		"AGENT_KEY":    fakeAPI(t, []string{"admin", "otto"}, nil),
		"OPERATOR_KEY": fakeAPI(t, []string{"admin"}, []string{"agent"}),
	} {
		done, err := setupFor(srv).alreadySetUp(context.Background(), envFile(t, fullEnv))
		if done || err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), resetHint) {
			t.Errorf("%s rejected: done=%v err=%v, want an error naming it with the reset instructions", name, done, err)
		}
	}
}

func TestNoEnvMeansSetUpFromScratch(t *testing.T) {
	srv := fakeAPI(t, nil, nil)
	done, err := setupFor(srv).alreadySetUp(context.Background(), filepath.Join(t.TempDir(), ".env"))
	if done || err != nil {
		t.Fatalf("done=%v err=%v, want a fresh setup", done, err)
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("alreadySetUp wrote a file")
	}
}

// .env is written only at the end, so a step that fails leaves a tenant no
// rerun can reuse: every step failure says how to start over.
func TestAFailedStepSaysHowToStartOver(t *testing.T) {
	err := stepError("policy", errors.New("HTTP 503"))
	if !strings.Contains(err.Error(), "policy") || !strings.Contains(err.Error(), "HTTP 503") ||
		!strings.Contains(err.Error(), resetHint) {
		t.Fatalf("stepError = %v", err)
	}
}
