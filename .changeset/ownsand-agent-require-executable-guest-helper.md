---
"ownsand-agent": patch
---

`ownsand-agent image build` now rejects a guest helper that has the right CPU but is not an executable, before it provisions the image. A macOS helper must be a Mach-O file of type `MH_EXECUTE` (an arm64 relocatable object or dynamic library is refused). A Windows helper must be an amd64 PE image with the executable flag and an optional header and without the DLL flag (a COFF object file or a DLL is refused). Valid executables built with `go build` for `GOOS=darwin GOARCH=arm64` and `GOOS=windows GOARCH=amd64` are still accepted, and wrong-architecture helpers are still refused.
