# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Runtime image of ownsand-api. The image build context contains the
# `dist/ownsand-api` binary (linux/amd64, CGO disabled), `servers/LICENSE` as
# `LICENSE` and the Go toolchain license as `GO-LICENSE`, like accounts-api.
# The `deploy` field of package.json makes the Droplet deployment publish UDP
# port 3478 for STUN and proxy h2c to the container (see
# docs/servers-ownsand-api-foundation.md, Runtime and Language).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY dist/ownsand-api /usr/local/bin/ownsand-api
# FSL-1.1-ALv2 terms and copyright notice (servers/LICENSE).
COPY LICENSE /usr/share/licenses/ownsand-api/LICENSE
# BSD-3-Clause notice of the statically linked Go standard library and runtime.
COPY GO-LICENSE /usr/share/licenses/ownsand-api/GO-LICENSE
# A loopback-bound STUN listener cannot receive datagrams that Docker forwards
# from a published UDP port, so the image listens on the wildcard address. The
# standalone binary keeps its loopback default; an explicit STUN_ADDR wins.
ENV STUN_ADDR=0.0.0.0:3478
EXPOSE 3478/udp
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ownsand-api"]
