# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Runtime image for the Droplet deployment. The deployment workflow does not
# deploy ownsand-api yet (it needs the public UDP port for STUN first; see
# docs/servers-ownsand-api-foundation.md). Build `dist/ownsand-api` for
# linux/amd64 with CGO disabled and place LICENSE (servers/LICENSE) and
# GO-LICENSE next to this file before `docker build`, like accounts-api.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY dist/ownsand-api /usr/local/bin/ownsand-api
# FSL-1.1-ALv2 terms and copyright notice (servers/LICENSE).
COPY LICENSE /usr/share/licenses/ownsand-api/LICENSE
# BSD-3-Clause notice of the statically linked Go standard library and runtime.
COPY GO-LICENSE /usr/share/licenses/ownsand-api/GO-LICENSE
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ownsand-api"]
