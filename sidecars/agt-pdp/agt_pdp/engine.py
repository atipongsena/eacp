"""EACP decisions from the pinned AGT policy layer and ACS engine.

The engine is stateless (ADR-002 §2): every request carries the policy
bundle EACP pinned, and the only memory is a bounded cache of ACS runtimes
keyed by bundle id, version and content digest. It never resolves an
escalation: it calls AgentControl.evaluate_intervention_point, never
enforce() or run(), so approval state stays in EACP (ADR-005).

Failure mapping (ADR-002 §6, §8): a bad request is 400, a bundle the local
provider rejects is 422 policy_invalid, a bundle ACS cannot express is 422
policy_unsupported, and every engine failure - an ACS runtime_error:*
verdict, OPA missing or slow, output that disagrees with the bundle - is 503.
None of these is a decision, so EACP keeps the action RECEIVED.
"""

import asyncio
import copy
import hashlib
import logging
import re
import shutil
import subprocess
import tempfile
import threading
import uuid
from collections import OrderedDict
from datetime import datetime, timezone
from functools import lru_cache
from importlib import metadata
from pathlib import Path
import json
import os

import rfc8785
import yaml
from agent_control_specification import EnforcementMode, InterventionPoint
from agt.policies.runtime import AgtRuntime

from .bundle import VERDICTS, Bundle, PolicyError, parse_bundle
from .strictjson import StrictJSONError, check

PROTOCOL = "eacp-agt-pdp/1"
PROVIDER = "microsoft-agt"
ADAPTER = (Path(__file__).parent / "eacp.rego").read_bytes()
ADAPTER_DIGEST = "sha256:" + hashlib.sha256(ADAPTER).hexdigest()

UUID_FIELDS = ("tenant", "agent", "agent_version")
TEXT_FIELDS = ("subject", "operation", "target", "tool", "tool_schema_version", "resource")
BINDING_FIELDS = set(UUID_FIELDS) | set(TEXT_FIELDS) | {"payload"}
REQUEST_FIELDS = {"protocol", "binding", "risk_class", "side_effect_class", "policy"}
UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
NIL_UUID = "00000000-0000-0000-0000-000000000000"

log = logging.getLogger("agt_pdp.engine")


class PDPError(Exception):
    def __init__(self, status, code, detail=""):
        super().__init__(f"{code}: {detail}")
        self.status = status
        self.code = code
        self.detail = detail


def _bad(detail):
    return PDPError(400, "bad_request", detail)


def _malformed(detail):
    return PDPError(503, "malformed_engine_output", detail)


@lru_cache(maxsize=1)
def _opa_version():
    opa = os.environ.get("ACS_OPA_PATH") or "opa"
    out = subprocess.run([opa, "version"], capture_output=True, text=True, timeout=10, check=True).stdout
    found = re.search(r"^Version:\s*v?(\S+)\s*$", out, re.MULTILINE)
    if not found:
        raise RuntimeError("cannot read the OPA version")
    return found.group(1)


def versions():
    """The engine stack that decides; EACP checks it against its pins."""
    return {"agt": metadata.version("agt-policies"), "acs": metadata.version("agent-control-specification"),
            "opa": _opa_version()}


def _uuid(value, what):
    if not isinstance(value, str) or not UUID_RE.match(value) or value == NIL_UUID:
        raise _bad(f"{what} must be a lowercase, non-nil UUID")
    return value


def _digest(binding, payload):
    doc = {k: binding[k] for k in BINDING_FIELDS if k != "payload"}
    doc["payload"] = payload
    return hashlib.sha256(rfc8785.dumps(doc)).hexdigest()


def _validate(req):
    if not isinstance(req, dict) or set(req) != REQUEST_FIELDS:
        raise _bad(f"a request has exactly {sorted(REQUEST_FIELDS)}")
    if req["protocol"] != PROTOCOL:
        raise _bad(f"protocol must be {PROTOCOL}")
    b = req["binding"]
    if not isinstance(b, dict) or set(b) != BINDING_FIELDS:
        raise _bad(f"a binding has exactly {sorted(BINDING_FIELDS)}")
    for f in UUID_FIELDS:
        _uuid(b[f], f)
    for f in TEXT_FIELDS:
        if not isinstance(b[f], str) or not b[f]:
            raise _bad(f"{f} must be a nonempty string")
    for f in ("risk_class", "side_effect_class"):
        if not isinstance(req[f], str):
            raise _bad(f"{f} must be a string")
    p = req["policy"]
    if not isinstance(p, dict) or set(p) != {"bundle_id", "version", "content"}:
        raise _bad("policy has exactly bundle_id, version and content")
    _uuid(p["bundle_id"], "policy.bundle_id")
    if isinstance(p["version"], bool) or not isinstance(p["version"], int) or p["version"] < 1:
        raise _bad("policy.version must be a positive integer")
    try:
        check(req)
        input_digest = _digest(b, b["payload"])
    except (StrictJSONError, rfc8785.CanonicalizationError) as exc:
        raise _bad(f"request is not I-JSON: {exc}") from None
    return input_digest


def decide(bundle: Bundle, payload, result):
    """Map an ACS result to (verdict, reason, enforced payload, approval,
    rule id), trusting nothing that disagrees with the bundle."""
    v = result.verdict
    decision = getattr(v.decision, "value", v.decision)
    reason = v.reason or ""
    if reason.startswith("runtime_error:"):
        raise PDPError(503, "pdp_runtime_error", reason)
    if decision not in VERDICTS:
        raise _malformed(f"unknown decision {decision!r}")
    pointers = getattr(v.evidence, "verification_pointers", None) or {}
    rule_id = pointers.get("eacp_rule_id")
    if rule_id is None:
        if decision != "deny" or reason != "no_matching_rule":
            raise _malformed("a decision without a matched rule must be the default deny")
        return "deny", reason, payload, None, None
    rule = bundle.rules.get(rule_id)
    if rule is None or rule.verdict != decision or rule.reason != reason:
        raise _malformed(f"the decision does not match rule {rule_id!r} of the bundle")
    enforced = payload
    if decision == "transform":
        t = v.transform
        if (t is None or t.path != "$policy_target" or not result.transformed_policy_target_applied
                or not isinstance(result.transformed_policy_target, dict)):
            raise _malformed("a transform must replace the whole payload object")
        enforced = result.transformed_policy_target
    approval = copy.deepcopy(rule.approval) if decision == "escalate" else None
    return decision, reason, enforced, approval, rule_id


class _Entry:
    def __init__(self, directory, runtime):
        self.directory = directory
        self.runtime = runtime
        self.users = 0
        self.evicted = False


class Engine:
    def __init__(self, instance_id, cache_size=64, work_dir=None):
        if not instance_id:
            raise ValueError("instance_id is required")
        self.instance_id = instance_id
        self.cache_size = cache_size
        self.work_dir = Path(work_dir) if work_dir else Path(tempfile.mkdtemp(prefix="agt-pdp-"))
        self.work_dir.mkdir(parents=True, exist_ok=True)
        self.runtimes_built = 0
        self._cache = OrderedDict()
        self._lock = threading.Lock()

    def cached_runtimes(self):
        with self._lock:
            return len(self._cache)

    def _build(self, key, bundle):
        directory = self.work_dir / hashlib.sha256(repr(key).encode()).hexdigest()[:32]
        shutil.rmtree(directory, ignore_errors=True)
        policy_dir = directory / "bundle" / "eacp"
        policy_dir.mkdir(parents=True)
        (policy_dir / "pdp.rego").write_bytes(ADAPTER)
        (policy_dir / "data.json").write_text(json.dumps(bundle.rego_data(), ensure_ascii=False), encoding="utf-8")
        manifest = {
            "agent_control_specification_version": "0.3.1-beta",
            "metadata": {"name": "eacp-agt-pdp", "description": "EACP local-provider rules evaluated by ACS"},
            "policies": {"eacp": {"type": "rego", "bundle": (directory / "bundle").as_posix(),
                                  "query": "data.eacp.pdp.verdict"}},
            "intervention_points": {"pre_tool_call": {
                "policy_target": "$snap.tool_call.args", "policy_target_kind": "tool_args",
                "policy": {"id": "eacp"}}},
        }
        path = directory / "manifest.yaml"
        path.write_text(yaml.safe_dump(manifest, sort_keys=False), encoding="utf-8")
        runtime = AgtRuntime(path)
        self.runtimes_built += 1
        return _Entry(directory, runtime)

    def _acquire(self, key, bundle):
        with self._lock:
            entry = self._cache.get(key)
            if entry is None:
                entry = self._build(key, bundle)
                self._cache[key] = entry
                while len(self._cache) > self.cache_size:
                    _, old = self._cache.popitem(last=False)
                    old.evicted = True
                    if old.users == 0:
                        self._drop(old)
            else:
                self._cache.move_to_end(key)
            entry.users += 1
            return entry

    def _release(self, entry):
        with self._lock:
            entry.users -= 1
            if entry.evicted and entry.users == 0:
                self._drop(entry)

    @staticmethod
    def _drop(entry):
        try:
            entry.runtime.close()
        finally:
            shutil.rmtree(entry.directory, ignore_errors=True)

    def evaluate(self, req):
        input_digest = _validate(req)
        b, p = req["binding"], req["policy"]
        try:
            bundle = parse_bundle(p["content"])
        except PolicyError as exc:
            raise PDPError(422, exc.code, exc.detail) from None
        key = (p["bundle_id"], p["version"], hashlib.sha256(rfc8785.dumps(p["content"])).hexdigest())
        snapshot = {
            "tool_call": {"name": b["tool"], "args": b["payload"]},
            "eacp": {**{k: b[k] for k in UUID_FIELDS + TEXT_FIELDS},
                     "risk_class": req["risk_class"], "side_effect_class": req["side_effect_class"]},
        }
        try:
            entry = self._acquire(key, bundle)
        except Exception as exc:  # noqa: BLE001 - an engine that cannot load is unavailable
            log.warning("acs runtime construction failed: %s", type(exc).__name__)
            raise PDPError(503, "pdp_runtime_error", f"runtime construction failed: {type(exc).__name__}") from None
        try:
            result = asyncio.run(entry.runtime.control.evaluate_intervention_point(
                InterventionPoint.PRE_TOOL_CALL, snapshot, EnforcementMode.ENFORCE))
        except Exception as exc:  # noqa: BLE001 - any evaluation failure is transient
            log.warning("acs evaluation failed: %s", type(exc).__name__)
            raise PDPError(503, "pdp_runtime_error", f"evaluation failed: {type(exc).__name__}") from None
        finally:
            self._release(entry)
        verdict, reason, enforced, approval, rule_id = decide(bundle, b["payload"], result)
        try:
            check(enforced)
            enforced_digest = _digest(b, enforced)
            stack = versions()
        except (StrictJSONError, rfc8785.CanonicalizationError) as exc:
            raise _malformed(f"enforced payload is not I-JSON: {exc}") from None
        except Exception as exc:  # noqa: BLE001 - an unreadable engine stack is unavailable
            raise PDPError(503, "pdp_runtime_error", f"engine versions: {type(exc).__name__}") from None
        return {
            "protocol": PROTOCOL,
            "verdict": verdict,
            "reasons": [reason],
            "enforced_payload": enforced,
            "input_digest": input_digest,
            "enforced_digest": enforced_digest,
            "policy_bundle_id": p["bundle_id"],
            "policy_version": p["version"],
            "provider": PROVIDER,
            "provider_instance_id": self.instance_id,
            "decision_id": str(uuid.uuid4()),
            "evaluated_at": datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z"),
            "approval": approval,
            "versions": stack,
            "evidence": {
                "acs_action_identity": result.enforced_identity or result.input_identity,
                "rule_id": rule_id,
                "adapter_digest": ADAPTER_DIGEST,
                "versions": stack,
            },
        }
