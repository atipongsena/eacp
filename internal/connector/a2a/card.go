package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"eacp/internal/governance"
	"eacp/internal/worker"
)

// displayMembers leave the certified definition: they change nothing an
// agent or a reviewer relies on (ADR-030 §1).
var displayMembers = []string{"iconUrl", "documentationUrl"}

// Discover fetches the Agent Card of the agent whose JSON-RPC interface is
// endpoint and returns it as the one delegate tool. The card must name that
// exact endpoint as a JSONRPC 1.0 interface, so a delegation only ever goes
// where the registry approved.
func (c *Client) Discover(ctx context.Context, endpoint string, secret worker.Secret) (worker.Discovery, error) {
	u, ok := parseEndpoint(endpoint)
	if !ok {
		return worker.Discovery{}, fail("invalid_endpoint", "endpoint is not an http(s) URL without credentials")
	}
	if secret.Reveal() == "" {
		return worker.Discovery{}, fail("no_credential", "no credential for the endpoint")
	}
	cardURL := u.Scheme + "://" + u.Host + CardPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return worker.Discovery{}, fail("invalid_endpoint", "build request")
	}
	req.Header.Set("Accept", "application/json")
	if err := secret.Authorize(req, nil); err != nil { // last: a SigV4 credential signs every header
		return worker.Discovery{}, fail("credential_signing", "sign the card request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return worker.Discovery{}, fail(contextClass(ctx), "fetch the card: request failed")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return worker.Discovery{}, fail("unauthorized", "card: HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return worker.Discovery{}, fail("http_"+strconv.Itoa(resp.StatusCode), "card: HTTP %d", resp.StatusCode)
	}
	body, err := readAll(resp.Body, maxCardBytes)
	if errors.Is(err, errTooLarge) {
		return worker.Discovery{}, fail("too_large", "the card exceeds %d bytes", maxCardBytes)
	}
	if err != nil {
		return worker.Discovery{}, fail(contextClass(ctx), "read the card")
	}
	return delegate(body, endpoint)
}

// delegate validates card and builds the delegate tool from it.
func delegate(card []byte, endpoint string) (worker.Discovery, error) {
	canon, err := governance.Canonicalize(card)
	if err != nil {
		return worker.Discovery{}, fail("card_invalid", "the card is not I-JSON")
	}
	// PostgreSQL's jsonb cannot hold U+0000.
	if bytes.Contains(canon, []byte(`\u0000`)) {
		return worker.Discovery{}, fail("card_invalid", "the card contains U+0000")
	}
	obj, ok := object(canon)
	if !ok {
		return worker.Discovery{}, fail("card_invalid", "the card is not an object")
	}
	name, ok := str(obj["name"])
	if !ok || name == "" {
		return worker.Discovery{}, fail("card_invalid", "the card has no name")
	}
	if err := validSkills(obj["skills"]); err != nil {
		return worker.Discovery{}, err
	}
	if err := boundInterface(obj["supportedInterfaces"], endpoint); err != nil {
		return worker.Discovery{}, err
	}

	display := map[string]json.RawMessage{}
	for _, k := range displayMembers {
		if v, ok := obj[k]; ok {
			display[k] = v
			delete(obj, k)
		}
	}
	def, err1 := recanonical(map[string]any{"name": "delegate", "inputSchema": json.RawMessage(PayloadSchema),
		"agentCard": obj})
	disp, err2 := recanonical(display)
	if err1 != nil || err2 != nil {
		return worker.Discovery{}, fail("card_invalid", "the card cannot be canonicalised")
	}
	if len(def) > maxDefinition || len(disp) > maxDisplay {
		return worker.Discovery{}, fail("too_large", "the card is too large to certify")
	}
	version, _ := str(obj["version"])
	info, _ := json.Marshal(map[string]string{"name": clip(name, 256), "version": clip(version, 256)})
	return worker.Discovery{ProtocolVersion: Version, ServerInfo: info,
		Tools: []worker.DiscoveredTool{{RemoteName: "delegate", Definition: def, Display: disp}}}, nil
}

// validSkills requires an array of at most 200 objects, each with a
// non-empty id unique in the card.
func validSkills(raw json.RawMessage) error {
	var skills []map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &skills) != nil || skills == nil && string(raw) != "[]" {
		return fail("card_invalid", "skills is not an array of objects")
	}
	if len(skills) > maxSkills {
		return fail("card_invalid", "the card lists more than %d skills", maxSkills)
	}
	seen := map[string]bool{}
	for _, s := range skills {
		id, ok := str(s["id"])
		if s == nil || !ok || id == "" || seen[id] {
			return fail("card_invalid", "a skill has no id or a duplicate one")
		}
		seen[id] = true
	}
	return nil
}

// boundInterface requires a JSONRPC 1.0 interface whose url is endpoint,
// byte for byte (ADR-030 §2).
func boundInterface(raw json.RawMessage, endpoint string) error {
	var ifaces []struct {
		URL             json.RawMessage `json:"url"`
		ProtocolBinding json.RawMessage `json:"protocolBinding"`
		ProtocolVersion json.RawMessage `json:"protocolVersion"`
	}
	if len(raw) != 0 && json.Unmarshal(raw, &ifaces) != nil {
		return fail("card_invalid", "supportedInterfaces is not an array of objects")
	}
	supported := false
	for _, i := range ifaces {
		binding, _ := str(i.ProtocolBinding)
		version, _ := str(i.ProtocolVersion)
		if binding != Binding || version != Version {
			continue
		}
		supported = true
		if u, _ := str(i.URL); u == endpoint {
			return nil
		}
	}
	if !supported {
		return fail("no_supported_interface", "the card offers no %s %s interface", Binding, Version)
	}
	return fail("interface_mismatch", "the card's %s %s interface is not the connector endpoint", Binding, Version)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
