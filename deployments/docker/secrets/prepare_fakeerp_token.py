"""Create the local Fake ERP verifier secret from the existing dev manifest."""

import json
import os
from pathlib import Path
import tempfile


directory = Path(__file__).resolve().parent
manifest = json.loads((directory / "connector-secrets.dev.json").read_text())
matches = [
    item
    for item in manifest["secrets"]
    if item.get("secret_ref") == "fakeerp" and item.get("host") == "fakeerp:8090"
]
if len(matches) != 1:
    raise SystemExit("expected one local Fake ERP credential")
token = matches[0].get("value")
if not isinstance(token, str) or not token or len(token) > 4096 or any(c.isspace() for c in token):
    raise SystemExit("local Fake ERP credential is missing or invalid")

fd, temporary = tempfile.mkstemp(prefix=".fakeerp-token-", dir=directory)
try:
    with os.fdopen(fd, "w") as output:
        output.write(token + "\n")
    # Docker Compose bind-mounts file-backed secrets; the nonroot ERP image
    # must be able to read this local-development-only source file.
    os.chmod(temporary, 0o644)
    os.replace(temporary, directory / "fakeerp-token.dev")
finally:
    if os.path.exists(temporary):
        os.unlink(temporary)
