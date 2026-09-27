#!/bin/sh
# DEVELOPMENT ONLY: configures the compose stack's dev-mode Vault for the
# worker (ADR-019 Rev 1.3). AppRole login, a policy that can only read
# secret/data/eacp/*, and the two KV v2 values of the Vault demo, copied from
# the local dev secret files. role_id and secret_id go to /approle, which only
# the worker mounts (read-only). Safe to run again against the same Vault.
set -eu

until vault status >/dev/null 2>&1; do sleep 1; done

vault auth list -format=json | grep -q '"approle/"' || vault auth enable approle

vault policy write eacp-worker - <<'POLICY'
path "secret/data/eacp/*" {
  capabilities = ["read"]
}
POLICY

# The worker logs in again at two thirds of the token's lease; its secret_id
# is reused, so it has no use limit.
vault write auth/approle/role/eacp-worker token_policies=eacp-worker token_ttl=1h token_max_ttl=1h \
  secret_id_ttl=0 secret_id_num_uses=0 >/dev/null

# The generated dev files may end in CRLF; the values themselves do not.
vault kv put -mount=secret eacp/fakeerp token="$(tr -d '\r\n' </run/secrets/fakeerp_token)" >/dev/null
vault kv put -mount=secret eacp/fakeerp-oauth client_secret="$(tr -d '\r\n' </run/secrets/fakeerp_oauth_client)" >/dev/null

umask 077
vault read -field=role_id auth/approle/role/eacp-worker/role-id >/approle/role_id
vault write -f -field=secret_id auth/approle/role/eacp-worker/secret-id >/approle/secret_id
# The worker runs as 65532 (distroless nonroot).
chown 65532:65532 /approle /approle/role_id /approle/secret_id
chmod 0500 /approle
chmod 0400 /approle/role_id /approle/secret_id
echo "vault-init: approle eacp-worker, policy eacp-worker, secret/eacp/fakeerp and secret/eacp/fakeerp-oauth written"
