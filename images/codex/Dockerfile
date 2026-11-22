FROM docker.io/node:22-bookworm-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee

ARG CODEX_VERSION=0.171.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git tar \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /opt/codex \
    && npm install --prefix /opt/codex --omit=dev "@openai/codex@${CODEX_VERSION}"

COPY images/codex/config.toml /etc/buddy-sbx/codex-config.toml
COPY images/codex/codex /usr/local/bin/codex
RUN chmod 0755 /usr/local/bin/codex && chown -R node:node /opt/codex

USER node
WORKDIR /workspace
ENV HOME=/home/sandbox
ENTRYPOINT ["/usr/local/bin/codex"]
