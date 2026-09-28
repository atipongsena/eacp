# The compose image holds every EACP binary and docker-compose selects the
# command; the release stage holds only what a deployment runs.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# A release sets VERSION (release.yml); every other build reports "dev".
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/atipongsena/eacp/internal/version.Version=${VERSION}" -o /out/ \
      ./cmd/controlplane-api ./cmd/execution-worker ./cmd/fakeerp ./cmd/fakemcp ./cmd/fakea2a ./cmd/eacpctl \
      ./cmd/llm-gateway ./cmd/fakellm \
    && mkdir -p /pki /client-key /client-jwks

# The published image (release.yml builds --target release): only the
# commands a deployment runs, never a fake service.
FROM gcr.io/distroless/static-debian12:nonroot AS release
COPY --from=build /out/controlplane-api /out/execution-worker /out/llm-gateway /out/eacpctl /
USER nonroot:nonroot

# The compose image (the default target): every binary, fakes included.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
# Mount point of the development PDP PKI volume, writable by nonroot so a
# fresh named volume inherits the ownership (docker-compose.yml, pdp-pki).
COPY --from=build --chown=65532:65532 /pki /pki
# Likewise the development private_key_jwt key and JWKS volumes (client-key).
COPY --from=build --chown=65532:65532 /client-key /client-key
COPY --from=build --chown=65532:65532 /client-jwks /client-jwks
USER nonroot:nonroot
