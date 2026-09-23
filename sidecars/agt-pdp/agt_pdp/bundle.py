"""EACP policy bundles (format_version 1) for the AGT provider.

Validation mirrors the local provider (internal/governance/local.go) and the
database check (eacp.local_policy_valid): a bundle the local provider can
evaluate is accepted, anything else is policy_invalid. One valid form cannot
be expressed in ACS, an escalate rule with replacements, because an ACS
escalate verdict never carries a transform; it is policy_unsupported and the
action fails closed (ADR-002 §8).
"""

from dataclasses import dataclass

MATCH_FIELDS = ("subject", "operation", "target", "tool", "risk_class", "side_effect_class")
RULE_KEYS = {"id", "match", "verdict", "reason", "set", "approval"}
APPROVAL_KEYS = {"quorum", "eligible_roles", "ttl_seconds"}
VERDICTS = {"allow", "warn", "deny", "escalate", "transform"}
MAX_SAFE_NUMBER = 2 ** 53


class PolicyError(ValueError):
    def __init__(self, code, detail):
        super().__init__(detail)
        self.code = code
        self.detail = detail


def _invalid(detail):
    return PolicyError("policy_invalid", detail)


@dataclass(frozen=True)
class Rule:
    id: str
    match: dict
    verdict: str
    reason: str
    set: dict
    approval: dict | None


@dataclass(frozen=True)
class Bundle:
    ordered: tuple
    rules: dict

    def rego_data(self):
        """The OPA data document the Rego adapter evaluates (data.eacp)."""
        out = []
        for r in self.ordered:
            rule = {"id": r.id, "match": dict(r.match), "verdict": r.verdict, "reason": r.reason}
            if r.verdict == "transform":
                rule["set"] = r.set
            out.append(rule)
        return {"rules": out}


def _integer(value, what):
    # The local provider decodes the JCS form, where 2.0 is 2 and true is
    # not a number.
    if isinstance(value, bool):
        raise _invalid(f"{what} must be an integer")
    if isinstance(value, int):
        return value
    if isinstance(value, float) and value.is_integer() and abs(value) < 1e21:
        return int(value)
    raise _invalid(f"{what} must be an integer")


def _safe_numbers(value):
    stack = [value]
    while stack:
        item = stack.pop()
        if isinstance(item, dict):
            stack.extend(item.values())
        elif isinstance(item, list):
            stack.extend(item)
        elif isinstance(item, (int, float)) and not isinstance(item, bool) and abs(item) > MAX_SAFE_NUMBER:
            return False
    return True


def parse_bundle(content) -> Bundle:
    if not isinstance(content, dict) or set(content) - {"format_version", "rules"}:
        raise _invalid("a bundle is an object with format_version and rules only")
    if _integer(content.get("format_version"), "format_version") != 1:
        raise _invalid("unsupported format_version")
    rules = content.get("rules")
    if not isinstance(rules, list) or not rules:
        raise _invalid("rules must be a nonempty array")
    ordered, by_id = [], {}
    for raw in rules:
        if not isinstance(raw, dict) or set(raw) - RULE_KEYS:
            raise _invalid("a rule is an object with id, match, verdict, reason, set and approval only")
        rule_id, reason, verdict = raw.get("id"), raw.get("reason"), raw.get("verdict")
        if not isinstance(rule_id, str) or not rule_id.strip() or rule_id in by_id:
            raise _invalid("rule ids must be unique nonblank strings")
        if not isinstance(reason, str) or not reason.strip():
            raise _invalid(f"rule {rule_id}: reason must be a nonblank string")
        if verdict not in VERDICTS:
            raise _invalid(f"rule {rule_id}: unknown verdict")
        match = raw.get("match") or {}
        if not isinstance(match, dict) or any(k not in MATCH_FIELDS or not isinstance(v, str) for k, v in match.items()):
            raise _invalid(f"rule {rule_id}: match holds only string {', '.join(MATCH_FIELDS)}")
        replacements = raw.get("set") or {}
        if not isinstance(replacements, dict) or "" in replacements:
            raise _invalid(f"rule {rule_id}: set must be an object with nonblank keys")
        if not _safe_numbers(replacements):
            raise _invalid(f"rule {rule_id}: set exceeds the interoperable numeric range")
        approval = raw.get("approval")
        if verdict in ("allow", "warn", "deny") and (replacements or approval is not None):
            raise _invalid(f"rule {rule_id}: {verdict} cannot transform or require approval")
        if verdict == "transform" and (not replacements or approval is not None):
            raise _invalid(f"rule {rule_id}: transform requires replacements only")
        if verdict == "escalate":
            approval = _approval(rule_id, approval)
        rule = Rule(rule_id, {k: v for k, v in match.items() if v != ""}, verdict, reason, replacements, approval)
        ordered.append(rule)
        by_id[rule_id] = rule
    for rule in ordered:
        if rule.verdict == "escalate" and rule.set:
            raise PolicyError("policy_unsupported",
                              f"rule {rule.id}: ACS cannot apply replacements to an escalate verdict (ADR-002 §8)")
    return Bundle(tuple(ordered), by_id)


def _approval(rule_id, approval):
    if not isinstance(approval, dict) or set(approval) != APPROVAL_KEYS:
        raise _invalid(f"rule {rule_id}: escalate requires approval with quorum, eligible_roles and ttl_seconds")
    quorum = _integer(approval["quorum"], "quorum")
    ttl = _integer(approval["ttl_seconds"], "ttl_seconds")
    if not 1 <= quorum <= 5 or not 1 <= ttl <= 86400 or approval["eligible_roles"] != ["approver"]:
        raise _invalid(f"rule {rule_id}: invalid approval requirement")
    return {"quorum": quorum, "eligible_roles": ["approver"], "ttl_seconds": ttl}
