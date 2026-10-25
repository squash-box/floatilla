# syntax=docker/dockerfile:1
# Two images from one file:
#   platform  control plane, launchers and the CLI
#   sandbox   the per-run container: Go supervisor (PID 1 under tini) + TS agent runtime

FROM golang:1.27-bookworm AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY gen gen
COPY internal internal
COPY cmd cmd
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM node:24-bookworm-slim AS agent-build
WORKDIR /opt/floatilla/agent
COPY runtime/agent/package.json runtime/agent/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --ignore-scripts
COPY runtime/agent/tsconfig.json ./
COPY runtime/agent/src src
RUN npx tsc -p . && npm prune --omit=dev

FROM gcr.io/distroless/static-debian12 AS platform
COPY --from=go-build /out/floatilla-control /out/floatilla-launcher /out/floatilla /usr/local/bin/
COPY packs /packs
ENV FLOATILLA_PACKS=/packs

FROM node:24-bookworm-slim AS sandbox
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl ripgrep procps make \
 && rm -rf /var/lib/apt/lists/*
COPY --from=agent-build /opt/floatilla/agent/package.json /opt/floatilla/agent/
COPY --from=agent-build /opt/floatilla/agent/node_modules /opt/floatilla/agent/node_modules
COPY --from=agent-build /opt/floatilla/agent/dist /opt/floatilla/agent/dist
COPY --from=go-build /out/floatilla-supervisor /usr/local/bin/
# A fresh named volume mounted at /workspace inherits this ownership.
RUN useradd --create-home --uid 1001 agent && mkdir /workspace && chown agent:agent /workspace
USER agent
WORKDIR /workspace
ENV GIT_AUTHOR_NAME=floatilla GIT_AUTHOR_EMAIL=agent@floatilla.local \
    GIT_COMMITTER_NAME=floatilla GIT_COMMITTER_EMAIL=agent@floatilla.local
ENTRYPOINT ["floatilla-supervisor"]
