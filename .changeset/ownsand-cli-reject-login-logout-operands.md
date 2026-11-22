---
"ownsand-cli": patch
---

`ownsand login` and `ownsand logout` now reject any operand, also after `--`, with a usage error (exit status 2). Before, they ignored the operands: `ownsand logout extra` deleted the stored tokens and `ownsand login extra` started a sign-in.
