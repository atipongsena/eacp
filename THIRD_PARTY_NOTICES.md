# Third-party notices

EACP is licensed under the Apache License 2.0 (see [LICENSE](LICENSE) and [NOTICE](NOTICE)). It builds on the
components below, each under its own license. Licenses were read from each component's own license file (Go modules,
in the module cache at the version `go.mod` pins) or its package metadata (PyPI); an expression such as
`Apache-2.0 AND MIT` means the component's license file covers parts under each.

## Go modules

Compiled into the EACP binaries and images. Versions are those `go.mod` pins.

| Module | Version | License | Source |
|---|---|---|---|
| `github.com/a2aproject/a2a-go/v2` | v2.6.0 | Apache-2.0 | https://pkg.go.dev/github.com/a2aproject/a2a-go/v2@v2.6.0 |
| `github.com/anthropics/anthropic-sdk-go` | v1.75.0 | MIT | https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go@v1.75.0 |
| `github.com/aws/aws-sdk-go-v2` | v1.47.1 | Apache-2.0 | https://pkg.go.dev/github.com/aws/aws-sdk-go-v2@v1.47.1 |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | https://pkg.go.dev/github.com/google/uuid@v1.6.0 |
| `github.com/jackc/pgx/v5` | v5.11.0 | MIT | https://pkg.go.dev/github.com/jackc/pgx/v5@v5.11.0 |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | Apache-2.0 AND MIT | https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk@v1.8.0 |
| `github.com/nats-io/nats-server/v2` | v2.15.0 | Apache-2.0 | https://pkg.go.dev/github.com/nats-io/nats-server/v2@v2.15.0 |
| `github.com/nats-io/nats.go` | v1.54.0 | Apache-2.0 | https://pkg.go.dev/github.com/nats-io/nats.go@v1.54.0 |
| `github.com/openai/openai-go/v3` | v3.66.0 | Apache-2.0 | https://pkg.go.dev/github.com/openai/openai-go/v3@v3.66.0 |
| `github.com/pressly/goose/v3` | v3.28.0 | MIT | https://pkg.go.dev/github.com/pressly/goose/v3@v3.28.0 |
| `github.com/spiffe/go-spiffe/v2` | v2.8.2 | Apache-2.0 | https://pkg.go.dev/github.com/spiffe/go-spiffe/v2@v2.8.2 |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.71.0 | Apache-2.0 AND BSD-3-Clause | https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.71.0 |
| `go.opentelemetry.io/otel` | v1.46.0 | Apache-2.0 AND BSD-3-Clause | https://pkg.go.dev/go.opentelemetry.io/otel@v1.46.0 |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` | v1.46.0 | Apache-2.0 AND BSD-3-Clause | https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.46.0 |
| `go.opentelemetry.io/otel/exporters/stdout/stdouttrace` | v1.46.0 | Apache-2.0 AND BSD-3-Clause | https://pkg.go.dev/go.opentelemetry.io/otel/exporters/stdout/stdouttrace@v1.46.0 |
| `go.opentelemetry.io/otel/sdk` | v1.46.0 | Apache-2.0 AND BSD-3-Clause | https://pkg.go.dev/go.opentelemetry.io/otel/sdk@v1.46.0 |
| `go.yaml.in/yaml/v3` | v3.0.5 | MIT AND Apache-2.0 | https://pkg.go.dev/go.yaml.in/yaml/v3@v3.0.5 |
| `google.golang.org/grpc` | v1.83.2 | Apache-2.0 | https://pkg.go.dev/google.golang.org/grpc@v1.83.2 |
| `github.com/Microsoft/go-winio` | v0.6.2 | MIT | https://pkg.go.dev/github.com/Microsoft/go-winio@v0.6.2 |
| `github.com/antithesishq/antithesis-sdk-go` | v0.8.0-default-no-op | MIT | https://pkg.go.dev/github.com/antithesishq/antithesis-sdk-go@v0.8.0-default-no-op |
| `github.com/aws/smithy-go` | v1.28.1 | Apache-2.0 | https://pkg.go.dev/github.com/aws/smithy-go@v1.28.1 |
| `github.com/bahlo/generic-list-go` | v0.2.0 | BSD-3-Clause | https://pkg.go.dev/github.com/bahlo/generic-list-go@v0.2.0 |
| `github.com/buger/jsonparser` | v1.1.2 | MIT | https://pkg.go.dev/github.com/buger/jsonparser@v1.1.2 |
| `github.com/cenkalti/backoff/v5` | v5.0.3 | MIT | https://pkg.go.dev/github.com/cenkalti/backoff/v5@v5.0.3 |
| `github.com/cespare/xxhash/v2` | v2.3.0 | MIT | https://pkg.go.dev/github.com/cespare/xxhash/v2@v2.3.0 |
| `github.com/coder/websocket` | v1.8.15 | ISC | https://pkg.go.dev/github.com/coder/websocket@v1.8.15 |
| `github.com/felixge/httpsnoop` | v1.1.0 | MIT | https://pkg.go.dev/github.com/felixge/httpsnoop@v1.1.0 |
| `github.com/go-jose/go-jose/v4` | v4.1.5 | Apache-2.0 | https://pkg.go.dev/github.com/go-jose/go-jose/v4@v4.1.5 |
| `github.com/go-logr/logr` | v1.4.4 | Apache-2.0 | https://pkg.go.dev/github.com/go-logr/logr@v1.4.4 |
| `github.com/go-logr/stdr` | v1.2.2 | Apache-2.0 | https://pkg.go.dev/github.com/go-logr/stdr@v1.2.2 |
| `github.com/google/go-tpm` | v0.9.8 | Apache-2.0 | https://pkg.go.dev/github.com/google/go-tpm@v0.9.8 |
| `github.com/google/jsonschema-go` | v0.4.3 | MIT | https://pkg.go.dev/github.com/google/jsonschema-go@v0.4.3 |
| `github.com/grpc-ecosystem/grpc-gateway/v2` | v2.30.0 | BSD-3-Clause | https://pkg.go.dev/github.com/grpc-ecosystem/grpc-gateway/v2@v2.30.0 |
| `github.com/invopop/jsonschema` | v0.14.0 | MIT | https://pkg.go.dev/github.com/invopop/jsonschema@v0.14.0 |
| `github.com/jackc/pgpassfile` | v1.0.0 | MIT | https://pkg.go.dev/github.com/jackc/pgpassfile@v1.0.0 |
| `github.com/jackc/pgservicefile` | v0.0.0-20240606120523-5a60cdf6a761 | MIT | https://pkg.go.dev/github.com/jackc/pgservicefile@v0.0.0-20240606120523-5a60cdf6a761 |
| `github.com/jackc/puddle/v2` | v2.2.2 | MIT | https://pkg.go.dev/github.com/jackc/puddle/v2@v2.2.2 |
| `github.com/klauspost/compress` | v1.20.0 | BSD-3-Clause AND Apache-2.0 AND MIT | https://pkg.go.dev/github.com/klauspost/compress@v1.20.0 |
| `github.com/mfridman/interpolate` | v0.0.2 | MIT | https://pkg.go.dev/github.com/mfridman/interpolate@v0.0.2 |
| `github.com/minio/highwayhash` | v1.0.4 | Apache-2.0 | https://pkg.go.dev/github.com/minio/highwayhash@v1.0.4 |
| `github.com/nats-io/jwt/v2` | v2.8.2 | Apache-2.0 | https://pkg.go.dev/github.com/nats-io/jwt/v2@v2.8.2 |
| `github.com/nats-io/nkeys` | v0.4.16 | Apache-2.0 | https://pkg.go.dev/github.com/nats-io/nkeys@v0.4.16 |
| `github.com/nats-io/nuid` | v1.0.1 | Apache-2.0 | https://pkg.go.dev/github.com/nats-io/nuid@v1.0.1 |
| `github.com/pb33f/ordered-map/v2` | v2.3.1 | Apache-2.0 | https://pkg.go.dev/github.com/pb33f/ordered-map/v2@v2.3.1 |
| `github.com/segmentio/asm` | v1.2.1 | MIT | https://pkg.go.dev/github.com/segmentio/asm@v1.2.1 |
| `github.com/segmentio/encoding` | v0.5.4 | MIT | https://pkg.go.dev/github.com/segmentio/encoding@v0.5.4 |
| `github.com/sethvargo/go-retry` | v0.4.0 | Apache-2.0 | https://pkg.go.dev/github.com/sethvargo/go-retry@v0.4.0 |
| `github.com/standard-webhooks/standard-webhooks/libraries` | v0.0.1 | MIT | https://pkg.go.dev/github.com/standard-webhooks/standard-webhooks/libraries@v0.0.1 |
| `github.com/tidwall/gjson` | v1.19.0 | MIT | https://pkg.go.dev/github.com/tidwall/gjson@v1.19.0 |
| `github.com/tidwall/match` | v1.1.1 | MIT | https://pkg.go.dev/github.com/tidwall/match@v1.1.1 |
| `github.com/tidwall/pretty` | v1.2.1 | MIT | https://pkg.go.dev/github.com/tidwall/pretty@v1.2.1 |
| `github.com/tidwall/sjson` | v1.2.5 | MIT | https://pkg.go.dev/github.com/tidwall/sjson@v1.2.5 |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause | https://pkg.go.dev/github.com/yosida95/uritemplate/v3@v3.0.2 |
| `go.opentelemetry.io/auto/sdk` | v1.2.1 | Apache-2.0 | https://pkg.go.dev/go.opentelemetry.io/auto/sdk@v1.2.1 |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace` | v1.46.0 | Apache-2.0 AND BSD-3-Clause | https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace@v1.46.0 |
| `go.opentelemetry.io/otel/metric` | v1.46.0 | Apache-2.0 AND BSD-3-Clause | https://pkg.go.dev/go.opentelemetry.io/otel/metric@v1.46.0 |
| `go.opentelemetry.io/otel/trace` | v1.46.0 | Apache-2.0 AND BSD-3-Clause | https://pkg.go.dev/go.opentelemetry.io/otel/trace@v1.46.0 |
| `go.opentelemetry.io/proto/otlp` | v1.11.0 | Apache-2.0 | https://pkg.go.dev/go.opentelemetry.io/proto/otlp@v1.11.0 |
| `go.uber.org/multierr` | v1.11.0 | MIT | https://pkg.go.dev/go.uber.org/multierr@v1.11.0 |
| `go.yaml.in/yaml/v4` | v4.0.0-rc.2 | MIT AND Apache-2.0 | https://pkg.go.dev/go.yaml.in/yaml/v4@v4.0.0-rc.2 |
| `golang.org/x/crypto` | v0.57.0 | BSD-3-Clause | https://pkg.go.dev/golang.org/x/crypto@v0.57.0 |
| `golang.org/x/net` | v0.58.0 | BSD-3-Clause | https://pkg.go.dev/golang.org/x/net@v0.58.0 |
| `golang.org/x/oauth2` | v0.36.0 | BSD-3-Clause | https://pkg.go.dev/golang.org/x/oauth2@v0.36.0 |
| `golang.org/x/sync` | v0.23.0 | BSD-3-Clause | https://pkg.go.dev/golang.org/x/sync@v0.23.0 |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | https://pkg.go.dev/golang.org/x/sys@v0.48.0 |
| `golang.org/x/text` | v0.42.0 | BSD-3-Clause | https://pkg.go.dev/golang.org/x/text@v0.42.0 |
| `golang.org/x/time` | v0.16.0 | BSD-3-Clause | https://pkg.go.dev/golang.org/x/time@v0.16.0 |
| `google.golang.org/genproto/googleapis/api` | v0.0.0-20260819154853-08b0e4226688 | Apache-2.0 | https://pkg.go.dev/google.golang.org/genproto/googleapis/api@v0.0.0-20260819154853-08b0e4226688 |
| `google.golang.org/genproto/googleapis/rpc` | v0.0.0-20260831171406-18b4a7587f8a | Apache-2.0 | https://pkg.go.dev/google.golang.org/genproto/googleapis/rpc@v0.0.0-20260831171406-18b4a7587f8a |
| `google.golang.org/protobuf` | v1.36.12 | BSD-3-Clause | https://pkg.go.dev/google.golang.org/protobuf@v1.36.12 |

## Python packages of the AGT sidecar

Installed into the sidecar image from `sidecars/agt-pdp/requirements.txt`, pinned by version and hash.

| Package | Version | License | Source |
|---|---|---|---|
| `agt-policies` | 5.0.0 | MIT | https://pypi.org/project/agt-policies/5.0.0/ |
| `agent-control-specification` | 0.3.1b1 | MIT | https://pypi.org/project/agent-control-specification/0.3.1b1/ |
| `rfc8785` | 0.1.4 | Apache-2.0 | https://pypi.org/project/rfc8785/0.1.4/ |
| `pyyaml` | 6.0.3 | MIT | https://pypi.org/project/pyyaml/6.0.3/ |
| `pydantic` | 2.13.5 | MIT | https://pypi.org/project/pydantic/2.13.5/ |
| `pydantic-core` | 2.46.5 | MIT | https://pypi.org/project/pydantic-core/2.46.5/ |
| `annotated-types` | 0.8.0 | MIT | https://pypi.org/project/annotated-types/0.8.0/ |
| `typing-inspection` | 0.4.4 | MIT | https://pypi.org/project/typing-inspection/0.4.4/ |
| `typing-extensions` | 4.16.0 | PSF-2.0 | https://pypi.org/project/typing-extensions/4.16.0/ |

## Design assets of the operator console

Embedded in `internal/ui/static/`; the console serves no font, image or SVG file.

| Component | License | Use |
|---|---|---|
| [Bootstrap Icons](https://github.com/twbs/icons) 1.13 (commit 6945b70), Copyright (c) 2019-2024 The Bootstrap Authors | MIT | 21 glyph outlines, transcribed as CSS `clip-path` in `app.css` |
| [Radix Colors](https://github.com/radix-ui/colors), Copyright (c) 2022 WorkOS | MIT | the light and dark colour values behind the semantic tokens in `app.css` |

## Binaries and images

| Component | Version | License | Where it is used |
|---|---|---|---|
| Open Policy Agent (`opa`) | 1.20.2 | Apache-2.0 | bundled in the AGT sidecar image (https://github.com/open-policy-agent/opa) |
| `gcr.io/distroless/static-debian12` | `nonroot` | Apache-2.0 (image project); Debian packages under their own licenses | base of the EACP image |
| `python` | `3.12-slim-bookworm` | PSF-2.0 (CPython); Debian packages under their own licenses | base of the AGT sidecar image |
| `golang` | `1.27-bookworm` | BSD-3-Clause (Go); Debian packages under their own licenses | build stage only, not shipped |
| `postgres` | `18-alpine` | PostgreSQL License | Docker Compose and tests; not shipped |
| `nats` | `2.15.0-alpine` | Apache-2.0 | Docker Compose; not shipped |
| `hashicorp/vault` | `2.1.1` | BUSL-1.1 | development Docker Compose only (the Vault credential demo); not shipped |
| `busybox` | `1.37` | GPL-2.0 | development Docker Compose only; not shipped |
| Helm | v4.3.0 | Apache-2.0 | downloaded by the chart tests and CI; not shipped |

## NOTICE files of Apache-2.0 components

Reproduced as section 4(d) of the Apache License 2.0 requires.

### `github.com/aws/aws-sdk-go-v2`

```text
AWS SDK for Go
Copyright 2015 Amazon.com, Inc. or its affiliates. All Rights Reserved.
Copyright 2014-2015 Stripe, Inc.
```

### `github.com/aws/smithy-go`

```text
Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
```

### `google.golang.org/grpc`

```text
Copyright 2014 gRPC authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

### `go.yaml.in/yaml/v3`

```text
Copyright 2011-2016 Canonical Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

### `go.yaml.in/yaml/v4`

```text
Copyright 2011-2016 Canonical Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```
