---
"ownsand-console": patch
"delino-accounts": patch
---

The global link rules (`a`, `a:hover`, `a:focus-visible`) now skip anchors that render a `delino-ui` Button (`a:where(:not(.dui-button))`, which keeps the specificity of the plain rules, so one-class link rules such as `.list-link` and `.button-link` still win). Before, the "Open billing" toast action in the OwnSand console took the link color on the accent background, so its text was unreadable (the same color in light mode). It now shows white text on the accent background in light and dark mode, and other links keep the link color and the hover underline. The `delino-accounts` stylesheet has the same fix, but it has no Button rendered as a link at this commit, so its effect there is covered by a stylesheet test only.
