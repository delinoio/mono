---
"ownsand-agent": patch
---

The launchd plist that `ownsand-agent service install` and `service print` write now keeps a carriage return in the executable path and in environment names and values: it is written as `&#13;`, so launchd reads back the original text instead of a line feed. Text that XML 1.0 cannot represent (a control character other than tab, line feed and carriage return, U+FFFE, U+FFFF or invalid UTF-8) is now an error before any file is written, so an existing plist stays untouched, and `service print` reports the same error.
