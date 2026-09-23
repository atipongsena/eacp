// Package fakeerp provides the credential-protected Slice A demo target.
// Its append log retains effects and audit evidence across process restarts.
package fakeerp

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type record struct {
	OperationKey      string    `json:"operation_key"`
	ExternalReference string    `json:"external_reference"`
	VisibleAt         time.Time `json:"visible_at"`
}

type audit struct {
	At           time.Time `json:"at"`
	Principal    string    `json:"principal"`
	TenantID     string    `json:"tenant_id,omitempty"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	OperationKey string    `json:"operation_key,omitempty"`
	Outcome      string    `json:"outcome"`
}

type event struct {
	Audit  audit   `json:"audit"`
	Record *record `json:"record,omitempty"`
}

type ERP struct {
	mu      sync.Mutex
	token   string
	path    string
	failed  bool
	records map[string][]record
	audit   []audit
}

var connectorName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// New loads a durable Fake ERP operation log. A corrupt log fails startup
// closed, because an empty map could falsely say an operation never ran.
func New(token, dataPath string) (http.Handler, error) {
	if token == "" || dataPath == "" {
		return nil, errors.New("fakeerp: credential and data path are required")
	}
	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fakeerp: open operation log: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("fakeerp: sync operation log: %w", err)
	}
	if err := syncDir(filepath.Dir(dataPath)); err != nil {
		return nil, err
	}
	e := &ERP{token: token, path: dataPath, records: map[string][]record{}}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var ev event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil || ev.Audit.At.IsZero() {
			return nil, errors.New("fakeerp: corrupt operation log")
		}
		e.apply(ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("fakeerp: read operation log: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/execute", e.execute)
	mux.HandleFunc("GET /v1/operations/{key}", e.lookup)
	mux.HandleFunc("GET /v1/audit", e.listAudit)
	return mux, nil
}

func (e *ERP) apply(ev event) {
	e.audit = append(e.audit, ev.Audit)
	if ev.Record != nil {
		e.records[ev.Record.OperationKey] = append(e.records[ev.Record.OperationKey], *ev.Record)
	}
}

// append writes and syncs before applying an effect in memory or replying.
// A failed write never becomes a definitive no-effect response.
func (e *ERP) append(ev event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(e.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	e.apply(ev)
	return nil
}

func (e *ERP) principal(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "unauthenticated"
	}
	got := strings.TrimPrefix(header, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(e.token)) == 1 {
		return "execution-worker"
	}
	return "unauthenticated"
}

func (e *ERP) begin(r *http.Request, key string) audit {
	path := r.URL.Path
	if strings.HasPrefix(path, "/v1/operations/") {
		path = "/v1/operations/{key}"
	}
	tenant := r.Header.Get("X-EACP-Tenant-ID")
	if _, err := uuid.Parse(tenant); err != nil {
		tenant = ""
	}
	if !validKey(key, tenant) {
		key = ""
	}
	return audit{At: time.Now().UTC(), Principal: e.principal(r), TenantID: tenant,
		Method: r.Method, Path: path, OperationKey: key}
}

func (e *ERP) log(ev event) bool {
	if e.failed {
		return false
	}
	if err := e.append(ev); err != nil {
		e.failed = true
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func errorJSON(w http.ResponseWriter, status int, class string) {
	writeJSON(w, status, map[string]string{"error_class": class})
}

func validKey(key, tenant string) bool {
	parts := strings.Split(key, ":")
	if len(parts) != 3 || parts[0] != "eacp" || parts[1] != tenant {
		return false
	}
	_, err1 := uuid.Parse(parts[1])
	_, err2 := uuid.Parse(parts[2])
	return err1 == nil && err2 == nil
}

func (e *ERP) execute(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	if e.principal(r) != "execution-worker" {
		ev := event{Audit: e.begin(r, "")}
		ev.Audit.Outcome = "unauthorized"
		ok := e.log(ev)
		e.mu.Unlock()
		if !ok {
			errorJSON(w, 503, "audit_unavailable")
			return
		}
		errorJSON(w, 401, "unauthorized")
		return
	}
	var request map[string]json.RawMessage
	b, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil || len(b) > 64<<10 || json.Unmarshal(b, &request) != nil {
		e.reject(w, r, "", 422, "validation")
		return
	}
	var tool string
	if json.Unmarshal(request["tool"], &tool) != nil {
		e.reject(w, r, "", 422, "validation")
		return
	}
	connector, operation, validTool := strings.Cut(tool, ".")
	if !validTool || !connectorName.MatchString(connector) || (operation != "create_po" && operation != "create_po_eventual") {
		e.reject(w, r, "", 422, "validation")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	native := key != ""
	if ref, ok := request["external_reference"]; ok {
		var correlation string
		if json.Unmarshal(ref, &correlation) != nil || (native && correlation != key) {
			e.reject(w, r, key, 422, "validation")
			return
		}
		if !native {
			key = correlation
		}
	}
	if !validKey(key, r.Header.Get("X-EACP-Tenant-ID")) {
		e.reject(w, r, key, 422, "validation")
		return
	}
	var payload struct {
		Scenario          string `json:"scenario"`
		DelayMS           int    `json:"delay_ms"`
		VisibilityDelayMS int    `json:"visibility_delay_ms"`
	}
	if json.Unmarshal(request["payload"], &payload) != nil || payload.DelayMS < 0 || payload.DelayMS > 5000 ||
		payload.VisibilityDelayMS < 0 || payload.VisibilityDelayMS > 5000 {
		e.reject(w, r, key, 422, "validation")
		return
	}
	if operation == "create_po" && (payload.Scenario == "delayed_visibility" || payload.VisibilityDelayMS != 0) {
		e.reject(w, r, key, 422, "validation")
		return
	}
	if native && len(e.records[key]) > 0 {
		rec := e.records[key][0]
		ev := event{Audit: e.begin(r, key)}
		ev.Audit.Outcome = "deduplicated"
		ok := e.log(ev)
		e.mu.Unlock()
		if !ok {
			errorJSON(w, 503, "audit_unavailable")
			return
		}
		writeJSON(w, 200, map[string]string{"external_reference": rec.ExternalReference})
		return
	}
	switch payload.Scenario {
	case "fail_before_execute":
		e.reject(w, r, key, 422, "validation")
		return
	case "rate_limit":
		e.reject(w, r, key, 429, "rate_limited")
		return
	case "5xx_before_effect":
		e.reject(w, r, key, 503, "fakeerp_5xx_before_effect")
		return
	case "outage":
		e.reject(w, r, key, 503, "fakeerp_outage_no_effect")
		return
	case "", "5xx_after_effect", "slow_response", "delayed_visibility", "execute_then_timeout", "execute_then_reset":
	default:
		e.reject(w, r, key, 422, "validation")
		return
	}
	parts := strings.Split(key, ":")
	ref := "PO-" + parts[2]
	if n := len(e.records[key]); n > 0 {
		ref += "-" + strconv.Itoa(n+1)
	}
	rec := record{OperationKey: key, ExternalReference: ref, VisibleAt: time.Now().UTC()}
	if operation == "create_po_eventual" && payload.VisibilityDelayMS > 0 {
		rec.VisibleAt = rec.VisibleAt.Add(time.Duration(payload.VisibilityDelayMS) * time.Millisecond)
	}
	ev := event{Audit: e.begin(r, key), Record: &rec}
	ev.Audit.Outcome = "effect_committed"
	ok := e.log(ev)
	e.mu.Unlock()
	if !ok {
		errorJSON(w, 503, "commit_ambiguous")
		return
	}
	switch payload.Scenario {
	case "5xx_after_effect":
		errorJSON(w, 503, "fakeerp_5xx_after_effect")
	case "execute_then_reset":
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
		errorJSON(w, 503, "response_lost")
	case "slow_response", "execute_then_timeout":
		d := time.Duration(payload.DelayMS) * time.Millisecond
		if d == 0 {
			d = 2 * time.Second
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(d):
		}
		writeJSON(w, 200, map[string]string{"external_reference": ref})
	default:
		writeJSON(w, 200, map[string]string{"external_reference": ref})
	}
}

// reject requires the ERP lock and releases it before writing the response.
func (e *ERP) reject(w http.ResponseWriter, r *http.Request, key string, status int, class string) {
	ev := event{Audit: e.begin(r, key)}
	ev.Audit.Outcome = class
	ok := e.log(ev)
	e.mu.Unlock()
	if !ok {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	errorJSON(w, status, class)
}

func (e *ERP) lookup(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	if e.principal(r) != "execution-worker" {
		e.reject(w, r, key, 401, "unauthorized")
		return
	}
	if !validKey(key, r.Header.Get("X-EACP-Tenant-ID")) {
		e.reject(w, r, key, 422, "validation")
		return
	}
	records := e.records[key]
	ev := event{Audit: e.begin(r, key)}
	ev.Audit.Outcome = "lookup"
	ok := e.log(ev)
	e.mu.Unlock()
	if !ok {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	// A duplicate correlation key is conflicting evidence even when one
	// effect is still hidden by the eventual-visibility scenario.
	if len(records) > 1 {
		errorJSON(w, 409, "conflict")
		return
	}
	var visible []record
	for _, rec := range records {
		if !time.Now().Before(rec.VisibleAt) {
			visible = append(visible, rec)
		}
	}
	switch len(visible) {
	case 0:
		errorJSON(w, 404, "not_found")
	case 1:
		writeJSON(w, 200, map[string]string{"external_reference": visible[0].ExternalReference})
	default:
		errorJSON(w, 409, "conflict")
	}
}

func (e *ERP) listAudit(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	if e.principal(r) != "execution-worker" {
		e.reject(w, r, "", 401, "unauthorized")
		return
	}
	ev := event{Audit: e.begin(r, "")}
	ev.Audit.Outcome = "audit_read"
	ok := e.log(ev)
	entries := append([]audit(nil), e.audit...)
	e.mu.Unlock()
	if !ok {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	writeJSON(w, 200, entries)
}
