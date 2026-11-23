# tally, plus the three CLIs its subscriptions collector asks (usage-probe's
# contract). Each CLI version is pinned so a rebuild never changes the answer
# the collector parses; a new version is a change to this file.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY VERSION ./
COPY cmd ./cmd
COPY internal ./internal
RUN version=$(cut -d' ' -f1 VERSION) \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$version" -o /out/tally ./cmd/tally

FROM node:22-bookworm-slim
ARG CLAUDE_CODE_VERSION=2.1.306
ARG CODEX_VERSION=0.163.0
ARG GROK_VERSION=1.0.67
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tini \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} @openai/codex@${CODEX_VERSION} \
 && npm cache clean --force
RUN curl -fsSL "https://x.ai/cli/grok-${GROK_VERSION}-linux-x86_64.gz" | gunzip > /usr/local/bin/grok \
 && chmod 755 /usr/local/bin/grok \
 && grok --version
COPY --from=build /out/tally /usr/local/bin/tally
# usage-probe's name still answers, for exec commands written for it.
RUN ln -s /usr/local/bin/tally /usr/local/bin/usage-probe
# The CLIs update themselves unless told not to; the image is the version.
# HOME is on the data volume so the CLIs' settings survive a restart; the
# folder is owned by the runtime user so the image also runs without one.
RUN mkdir -p /data && chown 1000:1000 /data
ENV DISABLE_AUTOUPDATER=1 \
    HOME=/data/home
EXPOSE 8080 8081
# uid 1000, the base image's node user, owns the data volume.
USER 1000:1000
ENTRYPOINT ["/usr/bin/tini", "--", "tally"]
CMD ["serve"]
