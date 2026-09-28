package api

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/finops"
	"github.com/atipongsena/eacp/internal/identity"
)

// finopsReader may read spend: admins manage it, operators watch it,
// auditors review it (as budgetReader).
var (
	finopsReader = budgetReader
	finopsAcker  = []string{"operator", "admin"}
)

// maxOTLPBody bounds an OTLP export, before and after gzip.
const maxOTLPBody = 4 << 20

// maxBillingBody bounds a billing import (finops.MaxBillingLines lines).
const maxBillingBody = 8 << 20

func (s *Server) registerFinOps(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("POST /v1/agent/otlp/v1/traces", s.agent(s.otlpTraces))
	mux.Handle("GET /v1/finops/dashboard", p(finopsReader, s.finopsDashboard))
	mux.Handle("GET /v1/finops/chargeback", p(finopsReader, s.finopsChargeback))
	mux.Handle("GET /v1/finops/usage", p(finopsReader, s.finopsUsage))
	mux.Handle("GET /v1/finops/prices", p(finopsReader, s.finopsPrices))
	mux.Handle("POST /v1/finops/prices", p(admin, s.finopsAddPrice))
	mux.Handle("POST /v1/finops/billing", p(admin, s.finopsImportBilling))
	mux.Handle("GET /v1/finops/soft-limits", p(finopsReader, s.finopsSoftLimits))
	mux.Handle("PUT /v1/finops/soft-limits/{account}", p(admin, s.finopsSetSoftLimit))
	mux.Handle("GET /v1/finops/alerts", p(finopsReader, s.finopsAlerts))
	mux.Handle("POST /v1/finops/alerts/{id}/ack", p(finopsAcker, s.finopsAcknowledge))
}

// otlpTraces is an OTLP/HTTP JSON trace receiver (ADR-025 §1). Usage is
// bound to the calling agent key. A malformed or refused usage span is
// counted in partialSuccess; binary Protobuf is refused with 415.
func (s *Server) otlpTraces(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported_media_type",
			"detail": "only OTLP/HTTP JSON (Content-Type: application/json) is accepted"})
		return nil
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxOTLPBody)
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return badRequest{"gzip: " + err.Error()}
		}
		defer zr.Close()
		body = io.LimitReader(zr, maxOTLPBody+1)
	default:
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported_media_type",
			"detail": "Content-Encoding must be gzip or absent"})
		return nil
	}
	data, err := io.ReadAll(body)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) || len(data) > maxOTLPBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large",
			"detail": fmt.Sprintf("an export is at most %d bytes", maxOTLPBody)})
		return nil
	}
	if err != nil {
		return badRequest{"body: " + err.Error()}
	}
	parsed, err := finops.ParseTraces(data)
	if err != nil {
		return badRequest{err.Error()}
	}
	in, err := s.finops.RecordSpans(r.Context(), c.TenantID, c.AgentVersionID, parsed.Spans)
	if err != nil {
		return err
	}
	resp := map[string]any{}
	if rejected := parsed.Rejected + in.Rejected; rejected > 0 {
		msg := parsed.Message
		if msg == "" {
			msg = in.Message
		}
		resp["partialSuccess"] = map[string]string{"rejectedSpans": strconv.Itoa(rejected), "errorMessage": msg}
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// period reads from and to (RFC 3339); the default is the UTC month to date.
func period(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	from, to := finops.MonthStart(now), now
	for name, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := r.URL.Query().Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return from, to, badRequest{name + " must be an RFC 3339 time"}
			}
			*dst = t
		}
	}
	return from, to, nil
}

func limitParam(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, badRequest{"limit must be a positive integer"}
	}
	return n, nil
}

func (s *Server) finopsDashboard(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	d, err := s.finops.Dashboard(r.Context(), actor(c))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (s *Server) finopsChargeback(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	from, to, err := period(r)
	if err != nil {
		return err
	}
	cb, err := s.finops.Chargeback(r.Context(), actor(c), from, to, r.URL.Query().Get("group_by"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, cb)
	return nil
}

func (s *Server) finopsUsage(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	from, to, err := period(r)
	if err != nil {
		return err
	}
	f := finops.UsageFilter{From: from, To: to}
	if f.Limit, err = limitParam(r); err != nil {
		return err
	}
	if v := r.URL.Query().Get("agent_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return badRequest{"agent_id must be a UUID"}
		}
		f.AgentID = &id
	}
	usage, err := s.finops.Usage(r.Context(), actor(c), f)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": usage})
	return nil
}

func (s *Server) finopsPrices(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	prices, err := s.finops.Prices(r.Context(), actor(c))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"prices": prices})
	return nil
}

func (s *Server) finopsAddPrice(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in finops.NewPrice
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.finops.AddPrice(r.Context(), actor(c), in)
	return created(w, p, err)
}

// finopsImportBilling takes {"lines": [...]}; all lines or none are
// recorded, and external ids already imported are counted as duplicates.
func (s *Server) finopsImportBilling(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Lines []finops.BillingLine `json:"lines"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBillingBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return badRequest{fmt.Sprintf("body: %v", err)}
	}
	im, err := s.finops.ImportBilling(r.Context(), actor(c), in.Lines)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, im)
	return nil
}

func (s *Server) finopsSoftLimits(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	limits, err := s.finops.SoftLimits(r.Context(), actor(c))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"soft_limits": limits})
	return nil
}

// finopsSetSoftLimit takes {"monthly_limit": "<decimal>" | null, "reason"}.
// null clears the limit.
func (s *Server) finopsSetSoftLimit(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	account, err := pathID(r, "account")
	if err != nil {
		return err
	}
	var in struct {
		MonthlyLimit *string `json:"monthly_limit"`
		Reason       string  `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	l, err := s.finops.SetSoftLimit(r.Context(), actor(c), account, in.MonthlyLimit, in.Reason)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, l)
	return nil
}

func (s *Server) finopsAlerts(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	limit, err := limitParam(r)
	if err != nil {
		return err
	}
	open := false
	if v := r.URL.Query().Get("open"); v != "" {
		if open, err = strconv.ParseBool(v); err != nil {
			return badRequest{"open must be true or false"}
		}
	}
	alerts, err := s.finops.Alerts(r.Context(), actor(c), open, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts})
	return nil
}

func (s *Server) finopsAcknowledge(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	a, err := s.finops.Acknowledge(r.Context(), actor(c), id, in.Reason)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a)
	return nil
}
