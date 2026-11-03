FROM node:24.22.1-bookworm@sha256:d862bb5d082c5ae0210ac07e8c0344cb8cf7bef6d78e9b8c50c179da923108ea AS node
FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61
RUN apt-get update && apt-get install -y --no-install-recommends git gh ca-certificates python3 python3-websocket && rm -rf /var/lib/apt/lists/*
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
ARG CODEX_VERSION=0.159.2
ARG CLAUDE_VERSION=2.1.286
RUN node /usr/local/lib/node_modules/npm/bin/npm-cli.js install -g @openai/codex@${CODEX_VERSION} @anthropic-ai/claude-code@${CLAUDE_VERSION}
COPY --chmod=755 paddock-agent paddock-codex-state paddock-claude-message /usr/local/bin/
RUN useradd --uid 1000 --create-home --shell /bin/bash sandbox && mkdir -p /sandbox && chown sandbox:sandbox /sandbox
COPY --chown=sandbox:sandbox codex-config.toml /home/sandbox/.codex/config.toml
COPY --chown=sandbox:sandbox claude-config.json /home/sandbox/.claude/.claude.json
USER sandbox
WORKDIR /sandbox
CMD ["/bin/bash", "-l"]
