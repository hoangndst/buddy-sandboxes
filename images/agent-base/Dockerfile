FROM docker.io/node:22-bookworm-slim AS node-source
FROM ghcr.io/astral-sh/uv:latest AS uv-source

FROM docker.io/debian:bookworm-slim

# Install system utilities, diagnostics, and Python runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    coreutils \
    curl \
    git \
    grep \
    gzip \
    jq \
    less \
    procps \
    psmisc \
    python3 \
    python3-pip \
    python3-venv \
    ripgrep \
    sed \
    tar \
    tini \
    unzip \
    wget \
    xz-utils \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Copy Node.js 22 LTS toolchain from official image
COPY --from=node-source /usr/local/bin /usr/local/bin
COPY --from=node-source /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=node-source /usr/local/include/node /usr/local/include/node

# Copy uv binary
COPY --from=uv-source /uv /usr/local/bin/uv
COPY --from=uv-source /uvx /usr/local/bin/uvx

# Configure sandbox user and directories
RUN groupadd -g 1000 sandbox \
    && useradd -u 1000 -g sandbox -d /home/sandbox -s /bin/bash -m sandbox \
    && mkdir -p /workspace /home/sandbox/.local/bin /workspace/bin /opt/sandbox \
    && chown -R sandbox:sandbox /workspace /home/sandbox

# Configure git safe directory globally
RUN git config --system --add safe.directory /workspace

# Install non-zero privilege guidance shims
COPY images/agent-base/shims/sudo /usr/local/bin/sudo
COPY images/agent-base/shims/apt-get /usr/local/bin/apt-get
COPY images/agent-base/entrypoint.sh /opt/sandbox/entrypoint.sh
COPY images/agent-base/run.sh /etc/buddy/agent/run.sh
RUN chmod 0755 /usr/local/bin/sudo /usr/local/bin/apt-get /opt/sandbox/entrypoint.sh /etc/buddy/agent/run.sh

ENV HOME="/home/sandbox" \
    USER="sandbox" \
    LOGNAME="sandbox" \
    PATH="/workspace/bin:/home/sandbox/.local/bin:/usr/local/bin:/usr/bin:/bin"

WORKDIR /workspace
USER sandbox

ENTRYPOINT ["/usr/bin/tini", "-g", "-s", "--", "/opt/sandbox/entrypoint.sh"]
CMD ["/bin/bash"]
