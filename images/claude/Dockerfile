FROM docker.io/node:22-bookworm-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee

ARG CLAUDE_CODE_VERSION=2.1.330
ARG TARGETARCH
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl git tar \
    && rm -rf /var/lib/apt/lists/* \
	&& mkdir -p /home/sandbox /workspace \
	&& chown -R node:node /home/sandbox /workspace

RUN set -eu; \
	case "${TARGETARCH}" in \
		amd64) platform=linux-x64 ;; \
		arm64) platform=linux-arm64 ;; \
		*) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
	esac; \
	manifest="https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION}/manifest.json"; \
	curl -fsSL "${manifest}" -o /tmp/claude-manifest.json; \
	checksum="$(node -p "require('/tmp/claude-manifest.json').platforms['${platform}'].checksum")"; \
	curl -fsSL "https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION}/${platform}/claude" -o /usr/local/bin/claude; \
	echo "${checksum}  /usr/local/bin/claude" | sha256sum -c -; \
	chmod 0755 /usr/local/bin/claude; \
	test "$(claude --version | awk '{print $1}')" = "${CLAUDE_CODE_VERSION}"; \
	rm /tmp/claude-manifest.json

USER node
WORKDIR /workspace
ENV HOME=/home/sandbox \
	DISABLE_UPDATES=1
ENTRYPOINT ["claude"]
