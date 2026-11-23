ARG BASE_IMAGE=buddy-sandboxes-base:local
FROM ${BASE_IMAGE}

ARG CLAUDE_VERSION=2.1.330

USER root
RUN mkdir -p /opt/claude \
    && npm install --prefix /opt/claude --omit=dev "@anthropic-ai/claude-code@${CLAUDE_VERSION}" \
    && chown -R sandbox:sandbox /opt/claude

COPY images/agent-claude/claude-wrapper.sh /usr/local/bin/claude
RUN chmod 0755 /usr/local/bin/claude

USER sandbox
WORKDIR /workspace

ENTRYPOINT ["/usr/bin/tini", "-g", "-s", "--", "/usr/local/bin/claude"]
CMD ["claude", "--dangerously-skip-permissions"]
