"""Create the local Fake ERP, Fake MCP and Fake A2A verifier secrets, and the Fake ERP's
OAuth client secret, from the existing dev manifest."""

import json
import os
from pathlib import Path
import tempfile


directory = Path(__file__).resolve().parent
manifest = json.loads((directory / "connector-secrets.dev.json").read_text())


def write_file(prefix, filename, value):
    fd, temporary = tempfile.mkstemp(prefix=f".{prefix}-", dir=directory)
    try:
        with os.fdopen(fd, "w") as output:
            output.write(value + "\n")
        # Docker Compose bind-mounts file-backed secrets; the nonroot image
        # must be able to read this local-development-only source file.
        os.chmod(temporary, 0o644)
        os.replace(temporary, directory / filename)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def write_verifier(secret_ref, host, filename):
    matches = [
        item
        for item in manifest["secrets"]
        if item.get("secret_ref") == secret_ref and item.get("host") == host
    ]
    # One credential per target, however many demo tenants it is bound to.
    values = {item.get("value") for item in matches}
    if not matches or len(values) != 1:
        raise SystemExit(f"expected one local {secret_ref} credential")
    token = values.pop()
    if not isinstance(token, str) or not token or len(token) > 4096 or any(c.isspace() for c in token):
        raise SystemExit(f"local {secret_ref} credential is missing or invalid")

    write_file(f"{secret_ref}-token", filename, token)


def write_oauth_client(secret_ref, client_id, filename):
    """The token endpoint verifies the client the worker authenticates as."""
    matches = [
        item["oauth2"]
        for item in manifest["secrets"]
        if item.get("secret_ref") == secret_ref and "oauth2" in item
    ]
    secrets = {o.get("client_secret") for o in matches if o.get("client_id") == client_id}
    if not matches or len(secrets) != 1 or len(matches) != sum(o.get("client_id") == client_id for o in matches):
        raise SystemExit(f"expected one local {secret_ref} OAuth client {client_id}")
    secret = secrets.pop()
    if not isinstance(secret, str) or not secret or len(secret) > 4096 or any(c.isspace() for c in secret):
        raise SystemExit(f"local {secret_ref} OAuth client secret is missing or invalid")
    write_file(f"{secret_ref}-oauth-client", filename, secret)


write_verifier("fakeerp", "fakeerp:8090", "fakeerp-token.dev")
write_verifier("fakemcp", "fakemcp:8091", "fakemcp-token.dev")
write_verifier("fakea2a", "fakea2a:8092", "fakea2a-token.dev")
write_oauth_client("fakeerp-jit", "eacp-worker", "fakeerp-oauth-client.dev")
