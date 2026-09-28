package api

import (
	"net/http"

	"github.com/atipongsena/eacp/internal/budget"
	"github.com/atipongsena/eacp/internal/identity"
)

// budgetReader may see budget accounts: admins manage them, operators
// watch them, auditors review them.
var budgetReader = []string{"admin", "operator", "auditor"}

func (s *Server) registerBudget(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("POST /v1/budgets", p(admin, s.createBudget))
	mux.Handle("GET /v1/budgets", p(budgetReader, s.listBudgets))
	mux.Handle("GET /v1/budgets/{id}", p(budgetReader, s.getBudget))
	mux.Handle("POST /v1/budgets/{id}/limit", p(admin, s.changeBudgetLimit))
	mux.Handle("POST /v1/budget-limit-changes/{id}/approve", p(admin, s.decideBudgetLimit(true)))
	mux.Handle("POST /v1/budget-limit-changes/{id}/reject", p(admin, s.decideBudgetLimit(false)))
}

func (s *Server) createBudget(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in budget.NewAccount
	if err := decode(r, &in); err != nil {
		return err
	}
	a, err := s.budgets.CreateAccount(r.Context(), actor(c), in)
	return created(w, a, err)
}

func (s *Server) listBudgets(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	accounts, err := s.budgets.List(r.Context(), actor(c))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"budgets": accounts})
	return nil
}

func (s *Server) getBudget(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	a, err := s.budgets.Get(r.Context(), actor(c), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a)
	return nil
}

// changeBudgetLimit takes {"limit": "<decimal>", "reason": "..."}. A
// decrease applies at once (201, APPLIED); an increase waits for a second
// admin (201, PROPOSED).
func (s *Server) changeBudgetLimit(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Limit  string `json:"limit"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	change, err := s.budgets.ChangeLimit(r.Context(), actor(c), id, in.Limit, in.Reason)
	return created(w, change, err)
}

func (s *Server) decideBudgetLimit(apply bool) handler {
	return func(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in reasonBody
		if err := decode(r, &in); err != nil {
			return err
		}
		change, err := s.budgets.DecideLimitChange(r.Context(), actor(c), id, apply, in.Reason)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, change)
		return nil
	}
}
