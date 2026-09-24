package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// call sends one API request with the caller's key and prints the JSON
// response indented. Non-2xx responses are errors carrying the status.
func call(ctx context.Context, getenv func(string) string, out io.Writer, method, path string, body any) error {
	key := getenv("EACP_API_KEY")
	if key == "" {
		return errors.New("EACP_API_KEY: required")
	}
	base := getenv("EACP_API_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case json.RawMessage:
		rd = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		fmt.Fprintf(out, "HTTP %d\n", resp.StatusCode)
		return nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil {
		_, err = out.Write(raw)
		return err
	}
	pretty.WriteByte('\n')
	_, err = pretty.WriteTo(out)
	return err
}

func runAgent(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: eacpctl agent register|list|inspect")
	}
	switch args[0] {
	case "register":
		fs := newFlags("agent register")
		name := fs.String("name", "", "agent slug")
		display := fs.String("display-name", "", "display name")
		env := fs.String("env", "", "development|staging|production")
		risk := fs.String("risk", "", "low|medium|high|critical")
		ownerP := fs.String("owner-principal", "", "owning principal id")
		ownerG := fs.String("owner-group", "", "owning group id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if (*ownerP == "") == (*ownerG == "") {
			return errors.New("exactly one of --owner-principal or --owner-group is required: every agent has an owner")
		}
		body := map[string]any{"name": *name, "display_name": *display, "environment": *env, "risk_class": *risk}
		if *ownerP != "" {
			body["owner_principal_id"] = *ownerP
		} else {
			body["owner_group_id"] = *ownerG
		}
		return call(ctx, getenv, out, "POST", "/v1/agents", body)
	case "list":
		return call(ctx, getenv, out, "GET", "/v1/agents", nil)
	case "inspect":
		if len(args) != 2 {
			return errors.New("usage: eacpctl agent inspect <id|name>")
		}
		return call(ctx, getenv, out, "GET", "/v1/agents/"+url.PathEscape(args[1]), nil)
	default:
		return errors.New("usage: eacpctl agent register|list|inspect")
	}
}

const connectorUsage = `usage:
  eacpctl connector register --name --endpoint --secret-ref [--protocol http]
  eacpctl connector circuit <connector-id>
  eacpctl connector disable|enable <connector-id> --reason <text>   (operator, ADR-022 §3)`

func runConnector(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(connectorUsage)
	}
	switch args[0] {
	case "register":
	case "circuit":
		if len(args) != 2 {
			return errors.New(connectorUsage)
		}
		return call(ctx, getenv, out, "GET", "/v1/connectors/"+url.PathEscape(args[1])+"/circuit", nil)
	case "disable", "enable":
		if len(args) < 2 {
			return errors.New(connectorUsage)
		}
		fs := newFlags("connector " + args[0])
		reason := fs.String("reason", "", "why (journaled)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *reason == "" || fs.NArg() != 0 {
			return errors.New(connectorUsage)
		}
		return call(ctx, getenv, out, "POST", "/v1/connectors/"+url.PathEscape(args[1])+"/circuit/"+args[0],
			map[string]any{"reason": *reason})
	default:
		return errors.New(connectorUsage)
	}
	fs := newFlags("connector register")
	name := fs.String("name", "", "connector slug")
	endpoint := fs.String("endpoint", "", "base URL")
	secretRef := fs.String("secret-ref", "", "name of the secret held by the execution worker")
	protocol := fs.String("protocol", "http", "protocol")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	return call(ctx, getenv, out, "POST", "/v1/connectors", map[string]any{
		"name": *name, "protocol": *protocol, "endpoint": *endpoint, "secret_ref": *secretRef})
}

func runAPI(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) < 2 || len(args) > 3 || !strings.HasPrefix(args[1], "/") {
		return errors.New("usage: eacpctl api <METHOD> <PATH> [JSON]")
	}
	var body any
	if len(args) == 3 {
		if !json.Valid([]byte(args[2])) {
			return errors.New("api: body is not valid JSON")
		}
		body = json.RawMessage(args[2])
	}
	return call(ctx, getenv, out, strings.ToUpper(args[0]), args[1], body)
}
