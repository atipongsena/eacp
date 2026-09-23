# EACP Rego adapter for the Agent Control Specification (ADR-002 Rev 2.4 §8).
#
# ACS evaluates this module at the pre_tool_call intervention point. The
# policy target is the submitted payload ($snap.tool_call.args); the EACP
# binding fields, risk_class and side_effect_class are in snapshot.eacp; the
# bundle's rules are OPA data (data.eacp.rules) in bundle order, with blank
# match fields already removed (they are wildcards).
#
# Semantics are the local provider's: the first rule whose every match field
# equals the snapshot wins; no match is deny/no_matching_rule. A transform
# replaces the top-level keys named in `set` (shallow, like the local
# provider) by replacing the whole policy target. The matched rule id rides
# in the verdict evidence so the sidecar can check it against the bundle.

package eacp.pdp

import rego.v1

rule_matches(rule) if {
	every key, want in rule.match {
		input.snapshot.eacp[key] == want
	}
}

matching contains i if {
	some i, rule in data.eacp.rules
	rule_matches(rule)
}

first := data.eacp.rules[min(matching)] if count(matching) > 0

evidence := {"verification_pointers": {"eacp_rule_id": first.id}}

default verdict := {"decision": "deny", "reason": "no_matching_rule"}

verdict := {"decision": first.verdict, "reason": first.reason, "evidence": evidence} if {
	first.verdict != "transform"
}

verdict := {
	"decision": "transform",
	"reason": first.reason,
	"evidence": evidence,
	"transform": {
		"path": "$policy_target",
		"value": object.union(object.remove(input.policy_target.value, object.keys(first.set)), first.set),
	},
} if {
	first.verdict == "transform"
	is_object(input.policy_target.value)
}

# Replacing keys of a payload that is not an object is an evaluation
# failure, as in the local provider, not a decision. The unknown decision
# makes ACS report runtime_error:policy_output_invalid, which the sidecar
# turns into a transient failure (503).
verdict := {"decision": "eacp_transform_needs_object_payload"} if {
	first.verdict == "transform"
	not is_object(input.policy_target.value)
}
