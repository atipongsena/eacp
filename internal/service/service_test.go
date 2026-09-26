package service_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"eacp/internal/config"
	"eacp/internal/service"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestStartRefusesRoleThatCanBypassRLS(t *testing.T) {
	db := pgtest.New(t)
	var out bytes.Buffer
	_, _, err := service.Start(context.Background(), "test", envFrom(map[string]string{
		"EACP_DATABASE_URL": db.AdminDSN, // superuser
	}), config.Options{RequireDatabase: true}, &out)
	if err == nil || !strings.Contains(err.Error(), "bypass") {
		t.Fatalf("Start err = %v, want refusal because the role bypasses RLS", err)
	}
}

func TestStartRefusesUnmigratedSchema(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	// Schema exists only after migrations; grant nothing and start as app.
	var out bytes.Buffer
	_, _, err := service.Start(ctx, "test", envFrom(map[string]string{
		"EACP_DATABASE_URL": db.AppDSN,
	}), config.Options{RequireDatabase: true}, &out)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("Start err = %v, want refusal on unmigrated schema", err)
	}
}

func TestStartRejectsInvalidConfig(t *testing.T) {
	var out bytes.Buffer
	if _, _, err := service.Start(context.Background(), "test", envFrom(nil),
		config.Options{RequireDatabase: true}, &out); err == nil {
		t.Fatal("Start accepted missing EACP_DATABASE_URL")
	}
}

func TestStartedServiceIsReadyAndNeverLogsDatabasePassword(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	if err := storage.MigrateUp(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	var out bytes.Buffer
	deps, stop, err := service.Start(ctx, "test", envFrom(map[string]string{
		"EACP_DATABASE_URL": db.AppDSN,
	}), config.Options{RequireDatabase: true}, &out)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stop()

	rec := httptest.NewRecorder()
	deps.Handler(http.NewServeMux()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d %s, want 200", rec.Code, rec.Body.String())
	}

	deps.Log.Error("simulated failure", "dsn", db.AppDSN)
	u, _ := url.Parse(db.AppDSN)
	password, _ := u.User.Password()
	if strings.Contains(out.String(), password) {
		t.Fatalf("database password leaked into logs:\n%s", out.String())
	}
}

func TestReadinessFailsWhenSchemaIsNotMigrated(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	if err := storage.MigrateUp(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	var out bytes.Buffer
	deps, stop, err := service.Start(ctx, "test", envFrom(map[string]string{
		"EACP_DATABASE_URL": db.AppDSN,
	}), config.Options{RequireDatabase: true}, &out)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stop()
	if err := storage.MigrateDownAll(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("MigrateDownAll: %v", err)
	}

	rec := httptest.NewRecorder()
	deps.Handler(http.NewServeMux()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503 when schema is behind", rec.Code)
	}
}

func TestServiceWithoutDatabaseIsReady(t *testing.T) {
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "fakeerp", envFrom(nil),
		config.Options{RequireDatabase: false}, &out)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stop()
	rec := httptest.NewRecorder()
	deps.Handler(http.NewServeMux()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", rec.Code)
	}
}

func TestStopEndsBackgroundTasksBeforeReleasingResources(t *testing.T) {
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "test", envFrom(nil),
		config.Options{RequireDatabase: false}, &out)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	started, finished := make(chan struct{}), make(chan struct{})
	deps.Background(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // still using its resources
		close(finished)
	})
	<-started
	stop()
	select {
	case <-finished:
	default:
		t.Fatal("stop returned before the background task finished")
	}
}

func TestRedactSecretsExtendsTheServiceLogger(t *testing.T) {
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "execution-worker", envFrom(nil),
		config.Options{AllowConnectorSecrets: true}, &out)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stop()
	deps.RedactSecrets("tok-canary-1", "tok-canary-2")
	deps.Log.Info("call failed: tok-canary-1", "err", "bad token tok-canary-2")
	if s := out.String(); strings.Contains(s, "tok-canary") || !strings.Contains(s, `"service":"execution-worker"`) {
		t.Fatalf("log = %s", s)
	}
}

func TestTheServiceLoggerRedactsValuesAddedLater(t *testing.T) {
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "execution-worker", envFrom(nil),
		config.Options{AllowConnectorSecrets: true}, &out)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stop()
	log := deps.Log
	deps.Redaction().Add("tok-canary-late", time.Now().Add(time.Hour))
	deps.RedactSecrets("tok-canary-perm")
	log.Info("minted", "value", "tok-canary-late tok-canary-perm")
	if strings.Contains(out.String(), "tok-canary") {
		t.Fatalf("a value added after the logger was taken leaked: %s", out.String())
	}
}
