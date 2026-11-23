# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Runtime image of tide-api. The image build context contains the
# `dist/tide-api` binary (linux/amd64, CGO disabled), `servers/LICENSE` as
# `LICENSE` and the Go toolchain license as `GO-LICENSE`, like accounts-api.
# The `deploy` field of package.json makes the Droplet deployment proxy h2c
# to the container (see docs/servers-tide-api-foundation.md, Runtime and
# Language).
#
# TODO(tide-api Claude Code layer): the contract requires the `claude` binary
# at a pinned version of at least 2.1.224 in this image, because the lease
# holder runs the Claude Code orchestrator as a child process. The layer that
# adds the orchestrator installs it here, with its version pin and license
# notice; this core layer runs no child process. Remove this note then.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY dist/tide-api /usr/local/bin/tide-api
# FSL-1.1-ALv2 terms and copyright notice (servers/LICENSE).
COPY LICENSE /usr/share/licenses/tide-api/LICENSE
# BSD-3-Clause notice of the statically linked Go standard library and runtime.
COPY GO-LICENSE /usr/share/licenses/tide-api/GO-LICENSE
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/tide-api"]
