---
"ownsand-agent": patch
---

`ownsand-agent image build` for Windows now detaches the ISO that it attached when it cannot read the drive letter of the ISO, for example after a `Get-Volume` failure, a canceled build or a malformed result; before, the ISO stayed attached. It also reads the attach state again after a failed `Mount-DiskImage`, runs the cleanup on a fresh bounded context, reports a failed cleanup together with the primary error, leaves an ISO that was attached before the build attached, and runs builds of the same ISO file one after another.
