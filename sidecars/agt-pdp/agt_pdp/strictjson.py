"""Strict JSON parsing: EACP's JCS parser rejects what this rejects.

Python's json module silently keeps the last of two duplicate keys, accepts
NaN and Infinity, turns 1e400 into inf and admits lone surrogates. Each of
those makes a digest ambiguous (ADR-002 §4), so each is an error here.
"""

import json
import math

MAX_DEPTH = 128


class StrictJSONError(ValueError):
    pass


def _pairs(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise StrictJSONError(f"duplicate key {key!r}")
        out[key] = value
    return out


def _constant(name):
    raise StrictJSONError(f"non-finite number {name}")


def loads(raw: bytes):
    try:
        text = raw.decode("utf-8")
        value = json.loads(text, object_pairs_hook=_pairs, parse_constant=_constant)
    except StrictJSONError:
        raise
    except (ValueError, RecursionError) as exc:
        raise StrictJSONError(str(exc)) from None
    check(value)
    return value


def check(value):
    """Reject non-finite numbers, lone surrogates and excessive nesting."""
    stack = [(value, 0)]
    while stack:
        item, depth = stack.pop()
        if depth > MAX_DEPTH:
            raise StrictJSONError("JSON nested too deeply")
        if isinstance(item, dict):
            for key, child in item.items():
                _check_str(key)
                stack.append((child, depth + 1))
        elif isinstance(item, list):
            stack.extend((child, depth + 1) for child in item)
        elif isinstance(item, str):
            _check_str(item)
        elif isinstance(item, float) and not math.isfinite(item):
            raise StrictJSONError("non-finite number")


def _check_str(s):
    try:
        s.encode("utf-8")
    except UnicodeEncodeError:
        raise StrictJSONError("invalid Unicode (lone surrogate)") from None
