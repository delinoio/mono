---
"accounts-api": patch
"ownsand-api": patch
"sandrun-api": patch
---

Ship `THIRD-PARTY-LICENSES.txt`, the license files of every third-party Go module that the server binary links, in `/usr/share/licenses/<service>/` of the container image. The image build fails when a linked module has no license file.
