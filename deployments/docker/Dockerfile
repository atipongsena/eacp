# One image containing every EACP binary; docker-compose selects the command.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ \
      ./cmd/controlplane-api ./cmd/execution-worker ./cmd/fakeerp ./cmd/fakemcp ./cmd/eacpctl \
    && mkdir -p /pki /client-key /client-jwks

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
# Mount point of the development PDP PKI volume, writable by nonroot so a
# fresh named volume inherits the ownership (docker-compose.yml, pdp-pki).
COPY --from=build --chown=65532:65532 /pki /pki
# Likewise the development private_key_jwt key and JWKS volumes (client-key).
COPY --from=build --chown=65532:65532 /client-key /client-key
COPY --from=build --chown=65532:65532 /client-jwks /client-jwks
USER nonroot:nonroot
