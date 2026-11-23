---
"delino-ui": patch
"ownsand-console": patch
"delino-accounts": patch
---

`SelectOption` now shows the text of interpolated children (for example `{os} base image`) in the trigger and in the option list instead of the raw option value, and accepts an explicit `label` that overrides the children text. An option without text still shows its value. The OwnSand Create sandbox Source select shows the readable names on every open.
