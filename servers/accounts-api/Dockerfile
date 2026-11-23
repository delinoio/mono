# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Runtime image for the Droplet deployment. The deploy workflow builds
# `dist/accounts-api` for linux/amd64 with CGO disabled before it uploads the
# image, so this image only copies the binary and the licenses. The workflow
# also stages `THIRD-PARTY-LICENSES.txt` (scripts/ci/server-notices.sh), the
# license files of the third-party Go modules that the binary links.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY dist/accounts-api /usr/local/bin/accounts-api
# FSL-1.1-ALv2 terms and copyright notice (servers/LICENSE).
COPY LICENSE /usr/share/licenses/accounts-api/LICENSE
# BSD-3-Clause notice of the statically linked Go standard library and runtime.
COPY GO-LICENSE /usr/share/licenses/accounts-api/GO-LICENSE
# Licenses of the third-party Go modules that the binary links.
COPY THIRD-PARTY-LICENSES.txt /usr/share/licenses/accounts-api/THIRD-PARTY-LICENSES.txt
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/accounts-api"]
