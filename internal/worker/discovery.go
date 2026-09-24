package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// DiscoveredTool is one tool definition a server listed, in the canonical
// form the database fingerprints (ADR-023 §3).
type DiscoveredTool struct {
	// RemoteName is the tool's exact MCP name.
	RemoteName string
	// Definition is the RFC 8785 text of the tool object minus its
	// display-only fields (title, icons, annotations.title).
	Definition string
	// Display is the RFC 8785 text of those display-only fields, keyed
	// "title", "icons" and "annotations.title".
	Display string
}

// RejectedTool is a listed definition the scanner refused, and why. A
// rejected tool is treated as not listed (ADR-023 §2).
type RejectedTool struct {
	RemoteName string `json:"remote_name"`
	Reason     string `json:"reason"`
}

// Discovery is one complete listing of an MCP server's tools.
type Discovery struct {
	// ProtocolVersion is the MCP revision the listing was made with.
	ProtocolVersion string
	// ServerInfo is the server's self-reported identity ({"name","version"}):
	// for display only, never for decisions.
	ServerInfo json.RawMessage
	Tools      []DiscoveredTool
	Rejected   []RejectedTool
}

// DiscoveryError is a failed discovery. Class is recorded with the scan
// (lower case, digits and underscores).
type DiscoveryError struct {
	Class string
	Err   error
}

func (e *DiscoveryError) Error() string {
	if e.Err == nil {
		return "discovery failed: " + e.Class
	}
	return fmt.Sprintf("discovery failed (%s): %v", e.Class, e.Err)
}

func (e *DiscoveryError) Unwrap() error { return e.Err }

// DiscoveryClass returns the scan error class of err: its DiscoveryError
// class, or "discovery_error" for anything else.
func DiscoveryClass(err error) string {
	var d *DiscoveryError
	if errors.As(err, &d) && d.Class != "" {
		return d.Class
	}
	return "discovery_error"
}

// Discoverer lists the tools of one MCP server. Only the execution worker
// holds the secret, and a Discoverer never sends it anywhere but endpoint
// (ADR-001 §3). Discover must respect ctx.
type Discoverer interface {
	Discover(ctx context.Context, endpoint string, secret Secret) (Discovery, error)
}
