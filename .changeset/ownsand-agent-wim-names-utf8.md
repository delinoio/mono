---
"ownsand-agent": patch
---

The Windows image build now reads the edition names of a Windows installation image as UTF-8 whatever the console code page of the host is. Windows PowerShell wrote the names in the OEM code page of a localized host (for example 932, 949 or 850), so an edition name with non-ASCII characters came back damaged, and selecting an edition by its name failed. Selecting an edition by index and the names of ASCII-only images are unchanged.
