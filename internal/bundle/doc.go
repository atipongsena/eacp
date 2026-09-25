// Package bundle plans and applies Governance-as-Code change sets
// (ADR-026).
//
// A bundle is a named desired state for part of the registry: connectors,
// their tools and contracts, agents, versions and allowlists. Plan diffs it
// against the registry in one snapshot and records a change set whose steps
// are the registry writes that would converge it. Submit runs the
// submit-stage steps as the submitter; a second person approves and runs
// the rest. PostgreSQL (migration 00020) binds each stage to its
// transaction and actor, computes every digest, refuses a stale change set
// and journals each move; the registry triggers still decide every step.
// Nothing is ever deleted, and nothing here grants more than the API does.
package bundle
