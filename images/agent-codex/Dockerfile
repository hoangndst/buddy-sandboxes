ARG BASE_IMAGE=buddy-sandboxes-base:local
FROM ${BASE_IMAGE}

ARG CODEX_VERSION=0.171.0

USER root
RUN mkdir -p /opt/codex /etc/buddy/codex \
    && npm install --prefix /opt/codex --omit=dev "@openai/codex@${CODEX_VERSION}" \
    && chown -R sandbox:sandbox /opt/codex

COPY images/agent-codex/config.toml /etc/buddy/codex/config.toml
COPY images/agent-codex/codex-wrapper.sh /usr/local/bin/codex
RUN chmod 0755 /usr/local/bin/codex /etc/buddy/codex/config.toml

USER sandbox
WORKDIR /workspace

ENTRYPOINT ["/usr/bin/tini", "-g", "-s", "--", "/usr/local/bin/codex"]
CMD ["codex"]
