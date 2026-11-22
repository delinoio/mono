---
"ownsand-agent": patch
---

The Windows image build VM now ends the closer goroutine of each COM1 console connection when that connection ends. Before, every redial of the console pipe while the build VM ran left one goroutine behind until the build ended. The build VM now uses the same console copy loop as the sandbox runtime.
