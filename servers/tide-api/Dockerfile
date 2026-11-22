# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Runtime image of tide-api. The image build context contains the
# `dist/tide-api` binary (linux/amd64, CGO disabled), `servers/LICENSE` as
# `LICENSE` and the Go toolchain license as `GO-LICENSE`, like accounts-api.
# The `deploy` field of package.json makes the Droplet deployment proxy h2c
# to the container (see docs/servers-tide-api-foundation.md, Runtime and
# Language).
#
# The image also contains the `claude` binary, because the lease holder runs
# one Claude Code orchestrator per CLAUDE_CODE integration as a child
# process (docs/servers-tide-api-foundation.md, Claude Code). The `claude`
# stage downloads the pinned native build from Anthropic's release bucket
# and verifies it as the Claude Code setup documentation describes
# (https://code.claude.com/docs/en/setup, "Binary integrity and code
# signing"): it checks the fingerprint of the release signing key, the
# detached signature of the release manifest and the SHA-256 that the
# manifest lists for linux-x64. The pinned SHA-256 below must equal the
# manifest value, so a changed manifest or binary also fails the build.
#
# Update: set CLAUDE_VERSION to a release of the stable channel
# (https://downloads.claude.ai/claude-code-releases/stable) that is at least
# 2.1.299 (the orchestrator flag --shutdown-drain-sec) and copy
# platforms["linux-x64"].checksum of its manifest.json into CLAUDE_SHA256.
# The native build needs glibc, so the runtime base is distroless/cc.
FROM debian:12-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS claude
ARG CLAUDE_VERSION=2.1.323
ARG CLAUDE_SHA256=604d29a6869409106b6a1110bdadcc8b72e8654a5a865d23258f938a696e348e
# Fingerprint of "Anthropic Claude Code Release Signing <security@anthropic.com>".
ARG CLAUDE_KEY_FINGERPRINT=31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
 && rm -rf /var/lib/apt/lists/*
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
WORKDIR /claude
RUN base="https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}"; \
    export GNUPGHOME=/claude/gnupg; mkdir -m 700 "$GNUPGHOME"; \
    curl -fsSL -o key.asc https://downloads.claude.ai/keys/claude-code.asc; \
    gpg --batch --import key.asc; \
    [ "$(gpg --batch --with-colons --fingerprint security@anthropic.com | awk -F: '$1 == "fpr" { print $10; exit }')" = "${CLAUDE_KEY_FINGERPRINT}" ]; \
    curl -fsSL -o manifest.json "${base}/manifest.json"; \
    curl -fsSL -o manifest.json.sig "${base}/manifest.json.sig"; \
    gpg --batch --status-fd 1 --verify manifest.json.sig manifest.json | grep -q "^\[GNUPG:\] VALIDSIG ${CLAUDE_KEY_FINGERPRINT} "; \
    tr -d '\n\r\t ' < manifest.json | grep -q "\"linux-x64\":{[^{}]*\"checksum\":\"${CLAUDE_SHA256}\""; \
    curl -fsSL -o claude "${base}/linux-x64/claude"; \
    echo "${CLAUDE_SHA256}  claude" | sha256sum -c -; \
    chmod 0755 claude; \
    ./claude --version | grep -q "^${CLAUDE_VERSION} "; \
    printf '%s\n' "Claude Code ${CLAUDE_VERSION} (https://code.claude.com), from ${base}/linux-x64/claude." \
      "(c) Anthropic PBC. All rights reserved. Use is subject to Anthropic's Commercial Terms of Service" \
      "(https://www.anthropic.com/legal/commercial-terms)." > NOTICE

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY dist/tide-api /usr/local/bin/tide-api
COPY --from=claude /claude/claude /usr/local/bin/claude
# FSL-1.1-ALv2 terms and copyright notice (servers/LICENSE).
COPY LICENSE /usr/share/licenses/tide-api/LICENSE
# BSD-3-Clause notice of the statically linked Go standard library and runtime.
COPY GO-LICENSE /usr/share/licenses/tide-api/GO-LICENSE
# The license notice of Claude Code (the notice of its npm package README).
COPY --from=claude /claude/NOTICE /usr/share/licenses/claude/NOTICE
# The pinned claude build is the only one that runs: the orchestrator gets
# DISABLE_AUTOUPDATER=1 from tide-api, and the image is read-only in
# /usr/local/bin for the nonroot user.
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/tide-api"]
