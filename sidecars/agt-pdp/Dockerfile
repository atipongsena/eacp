# EACP AGT sidecar PDP (ADR-002 Rev 2.4 §8). Build context: repository root.
#   docker build -f sidecars/agt-pdp/Dockerfile --target test .   # conformance + unit tests
#   docker build -f sidecars/agt-pdp/Dockerfile .                 # runtime image
# Every input is pinned: the base image by tag, Python packages (including
# the ACS engine wheel) by hash, and the OPA binary by checksum.

FROM python:3.14-slim-bookworm AS runtime
ADD --checksum=sha256:69da5179ee403d10fa11bab6cfb4ffb0d23dba5f9b682fa977db772a1da5670f \
    https://github.com/open-policy-agent/opa/releases/download/v1.20.2/opa_linux_amd64_static /usr/local/bin/opa
RUN chmod 0755 /usr/local/bin/opa
COPY sidecars/agt-pdp/requirements.txt /req/requirements.txt
RUN pip install --no-cache-dir --disable-pip-version-check --require-hashes --no-deps -r /req/requirements.txt \
    && pip check \
    && rm -rf /req
WORKDIR /app
COPY sidecars/agt-pdp/agt_pdp /app/agt_pdp
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 ACS_OPA_PATH=/usr/local/bin/opa ACS_OPA_TIMEOUT_MS=2000 \
    AGT_PDP_LISTEN=127.0.0.1:8181
# The same unprivileged uid as the distroless EACP images, so a shared
# development PKI volume is readable by both.
USER 65532:65532
EXPOSE 8181 8443
ENTRYPOINT ["python", "-m", "agt_pdp"]

# `--target test` runs the sidecar suites, including the ADR-002 conformance
# reference set, through the real pinned AGT/ACS/OPA stack.
FROM runtime AS test
COPY sidecars/agt-pdp/tests /app/tests
COPY test/conformance/governance_reference.json /app/conformance/governance_reference.json
ENV EACP_CONFORMANCE_REFERENCE=/app/conformance/governance_reference.json
RUN python -m unittest discover -s /app/tests -t /app -v

FROM runtime
