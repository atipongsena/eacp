// Command eacpctl is the EACP operator CLI.
//
// Schema (EACP_DATABASE_URL must be the schema owner's DSN):
//
//	eacpctl migrate up        apply pending migrations
//	eacpctl migrate status    print current and latest versions
//	eacpctl migrate down-all --yes-destroy-all-data
//	                          roll back everything; requires EACP_ENV=development|test
//
// Bootstrap (schema owner DSN; the break-glass trust root, ADR-003):
//
//	eacpctl key generate --kind agent|principal --tenant <uuid>
//	eacpctl tenant create --slug <slug> --name <name> [--id <uuid>]
//	        --admin name=<n>,subject=<s>,credential=<uuid>,hash=<hex>   (at least two)
//
// Registry, through the API (EACP_API_URL, EACP_API_KEY):
//
//	eacpctl agent register --name --display-name --env --risk (--owner-principal|--owner-group) <uuid>
//	eacpctl agent list
//	eacpctl agent inspect <id|name>
//	eacpctl connector register --name --endpoint --secret-ref [--protocol http]
//	eacpctl connector circuit <connector-id>
//	eacpctl connector disable|enable <connector-id> --reason <text>
//	                          stop or resume new dispatch to a connector (operators, ADR-022)
//	eacpctl connector tools|mcp <connector-id>
//	eacpctl connector scans <connector-id> [--limit N]
//	eacpctl connector scan <connector-id> --reason <text>
//	                          discovered tools, MCP scan state and history, rescan request (ADR-023)
//	eacpctl tool get|definitions <tool-id>
//	eacpctl tool quarantine|release <tool-id> --reason <text>
//	                          block a tool, or lift its quarantine (a second registry approver)
//	eacpctl dependency blast-radius mcp|tool|agent_version <uuid>
//	eacpctl dependency blast-radius model|system <name>
//	eacpctl kill activate|resume tenant|team|agent|agent_version|action|connector|tool|model|run <uuid> --reason <text> [--code <AGT reason>]
//	eacpctl kill list
//	eacpctl fleet status|list [--environment E] [--risk R] [--owner-group UUID] [--health H] [--window 24h]
//	eacpctl fleet operation <operation-id>
//	eacpctl fleet pause|quarantine [--agent NAME]... [--environment E] [--risk R] [--owner-group UUID]
//	                               [--tool CONNECTOR.TOOL] [--all] --reason <text> [--dry-run]
//	eacpctl fleet resume|release <operation-id> --reason <text> [--dry-run]
//	eacpctl fleet rollback <agent> [--to <version-id>] --reason <text> [--dry-run]
//	                          atomic lifecycle changes across agents (ADR-024)
//	eacpctl bundle validate|plan|deploy [-C DIR] [-t TARGET] [--var NAME=VALUE]... [--prune] [--dry-run]
//	eacpctl bundle approve|status|reject <change-set-id> [--reason TEXT]
//	eacpctl bundle list|drift [-C DIR] [-t TARGET]
//	                          Governance-as-Code: plan, two-person apply and drift (ADR-026)
//	eacpctl finops dashboard|prices|soft-limits
//	eacpctl finops chargeback [--by agent|team|account] [--from T] [--to T]
//	eacpctl finops usage [--agent UUID] [--from T] [--to T] [--limit N]
//	eacpctl finops price add --provider --model --unit --input --output [--cached] [--effective-from] --reason <text>
//	eacpctl finops billing import <file.json>
//	eacpctl finops soft-limit <account-id> (--limit X | --clear) --reason <text>
//	eacpctl finops alerts [--open] [--limit N]
//	eacpctl finops ack <alert-id> --reason <text>
//	                          LLM cost, chargeback, soft budgets and alerts (ADR-025)
//	eacpctl release list [--agent UUID] [--state S]
//	eacpctl release show <release-id>
//	eacpctl release open --candidate <version-id> --suite NAME... --reason <text>
//	                     [--min-replay N] [--min-shadow N] [--steps BP,...] [--min-canary-actions N]
//	eacpctl release evaluation <release-id> --suite --score --threshold --dataset-digest --evidence
//	eacpctl release advance <release-id> --from STATE [--from-bp N] --reason <text>
//	eacpctl release rollback <release-id> --reason <text>
//	                          evaluation, replay, shadow, canary and rollback (ADR-018)
//	eacpctl incident list [--state S] [--severity S] [--kind K] [--limit N]
//	eacpctl incident show <incident-id>
//	eacpctl incident open --title T --severity S --reason R [--subject-type K --subject-id <uuid>]
//	eacpctl incident ack <incident-id> --reason R
//	eacpctl incident assign <incident-id> <principal-uuid|none>
//	eacpctl incident note <incident-id> --text T
//	eacpctl incident link <incident-id> <kind> <uuid>
//	eacpctl incident resolve <incident-id> --code C --reason R
//	eacpctl soc summary      incidents and the Agent SOC read model (ADR-027)
//
// Actions and human resolution, through the API (operators, ADR-004 T35-T37):
//
//	eacpctl action list --state NEEDS_HUMAN_RESOLUTION [--limit N]
//	eacpctl action get|evidence <action-id>
//	eacpctl action resolve <action-id> --outcome succeeded|failed|retry --reason <text>
//	        [--evidence <text>] [--external-reference <ref>]
//	eacpctl action confirm|withdraw <action-id> <resolution-id> --reason <text>
//	                          a retry applies only when a second operator confirms it
//
//	eacpctl api <METHOD> <PATH> [JSON]      any other API call
//
// Development only (EACP_ENV=development|test):
//
//	eacpctl pdp-dev-certs --dir <dir> --name <host> [--name <host>...]
//	                          mutual-TLS PKI for the AGT sidecar PDP (ADR-002 §8)
//	eacpctl dev-client-key --dir <dir> --jwks <file> [--kid <kid>]
//	                          private_key_jwt key, certificate and JWKS for the Fake ERP (ADR-019 Rev 1.2)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/atipongsena/eacp/internal/version"
)

const usage = `usage:
  eacpctl version
  eacpctl migrate up|status|down-all --yes-destroy-all-data
  eacpctl key generate --kind agent|principal --tenant <uuid>
  eacpctl tenant create --slug <slug> --name <name> --admin <spec> --admin <spec>
  eacpctl agent register|list|inspect ...
  eacpctl connector register|circuit|disable|enable|tools|mcp|scans|scan ...
  eacpctl tool get|definitions|quarantine|release ...
  eacpctl dependency blast-radius <kind> <id|name>
  eacpctl llm-model register|list ...
  eacpctl llm-calls list|show ...
  eacpctl kill activate|resume <scope> <uuid> --reason <text> [--code <AGT reason>]
  eacpctl kill list
  eacpctl fleet status|list|operation|pause|resume|quarantine|release|rollback ...
  eacpctl bundle validate|plan|deploy|approve|reject|status|list|drift ...
  eacpctl finops dashboard|chargeback|usage|prices|price|billing|soft-limits|soft-limit|alerts|ack ...
  eacpctl release list|show|open|evaluation|advance|rollback ...
  eacpctl incident list|show|open|ack|assign|note|link|resolve ...
  eacpctl soc summary
  eacpctl action list|get|evidence|resolve|confirm|withdraw ...
  eacpctl api <METHOD> <PATH> [JSON]
  eacpctl pdp-dev-certs --dir <dir> --name <host> [--name <host>...]
  eacpctl dev-client-key --dir <dir> --jwks <file> [--kid <kid>]`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "eacpctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "version":
		_, err := fmt.Fprintln(out, version.Version)
		return err
	case "migrate":
		return runMigrate(ctx, args[1:], getenv, out)
	case "key":
		return runKey(args[1:], out)
	case "tenant":
		return runTenant(ctx, args[1:], getenv, out)
	case "agent":
		return runAgent(ctx, args[1:], getenv, out)
	case "connector":
		return runConnector(ctx, args[1:], getenv, out)
	case "tool":
		return runTool(ctx, args[1:], getenv, out)
	case "action":
		return runAction(ctx, args[1:], getenv, out)
	case "dependency":
		return runDependency(ctx, args[1:], getenv, out)
	case "llm-model":
		return runLLMModel(ctx, args[1:], getenv, out)
	case "llm-calls":
		return runLLMCalls(ctx, args[1:], getenv, out)
	case "kill":
		return runKill(ctx, args[1:], getenv, out)
	case "fleet":
		return runFleet(ctx, args[1:], getenv, out)
	case "bundle":
		return runBundle(ctx, args[1:], getenv, out)
	case "finops":
		return runFinOps(ctx, args[1:], getenv, out)
	case "release":
		return runRelease(ctx, args[1:], getenv, out)
	case "incident":
		return runIncident(ctx, args[1:], getenv, out)
	case "soc":
		return runSOC(ctx, args[1:], getenv, out)
	case "api":
		return runAPI(ctx, args[1:], getenv, out)
	case "pdp-dev-certs":
		return runPDPDevCerts(args[1:], getenv, out)
	case "dev-client-key":
		return runDevClientKey(args[1:], getenv, out)
	default:
		return errors.New(usage)
	}
}
