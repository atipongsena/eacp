package api

import (
	"net/http"
	"strconv"

	"eacp/internal/identity"
)

// registerMCP mounts the MCP registry routes (ADR-023): scan state and
// history, discovered tools and their definition history, rescan requests
// and quarantine. PostgreSQL enforces every rule; the role sets here only
// fail fast.
func (s *Server) registerMCP(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("GET /v1/connectors/{id}/tools", p(anyPrincipal, s.listTools))
	mux.Handle("GET /v1/connectors/{id}/mcp", p(anyPrincipal, s.mcpServer))
	mux.Handle("GET /v1/connectors/{id}/mcp/scans", p(anyPrincipal, s.mcpScans))
	mux.Handle("POST /v1/connectors/{id}/mcp/scan", p([]string{"operator", "registry_editor"}, s.requestScan))
	mux.Handle("GET /v1/tools/{id}", p(anyPrincipal, s.getTool))
	mux.Handle("GET /v1/tools/{id}/definitions", p(anyPrincipal, s.toolDefinitions))
	mux.Handle("POST /v1/tools/{id}/quarantine", p(containment, s.quarantineTool))
	mux.Handle("POST /v1/tools/{id}/release", p(approver, s.releaseTool))
}

func (s *Server) listTools(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	tools, err := s.reg.ListTools(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
	}
	return err
}

func (s *Server) getTool(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	tool, err := s.reg.GetTool(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, tool)
	}
	return err
}

func (s *Server) toolDefinitions(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	defs, err := s.reg.ToolDefinitions(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"definitions": defs})
	}
	return err
}

func (s *Server) mcpServer(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	srv, err := s.reg.MCPServer(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, srv)
	}
	return err
}

// mcpScans is GET /v1/connectors/{id}/mcp/scans?limit=, newest first.
func (s *Server) mcpScans(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return badRequest{"limit must be between 1 and 100"}
		}
		limit = n
	}
	scans, err := s.reg.MCPScans(r.Context(), actor(c), id, limit)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"scans": scans})
	}
	return err
}

func (s *Server) requestScan(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	srv, err := s.reg.RequestScan(r.Context(), actor(c), id, in.Reason)
	if err == nil {
		writeJSON(w, http.StatusOK, srv)
	}
	return err
}

func (s *Server) quarantineTool(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	return s.setQuarantine(w, r, c, true)
}

func (s *Server) releaseTool(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	return s.setQuarantine(w, r, c, false)
}

func (s *Server) setQuarantine(w http.ResponseWriter, r *http.Request, c identity.Caller, quarantine bool) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	set := s.reg.ReleaseTool
	if quarantine {
		set = s.reg.QuarantineTool
	}
	tool, err := set(r.Context(), actor(c), id, in.Reason)
	if err == nil {
		writeJSON(w, http.StatusOK, tool)
	}
	return err
}
