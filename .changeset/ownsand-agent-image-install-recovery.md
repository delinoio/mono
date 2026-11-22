---
"ownsand-agent": patch
---

An `ownsand-agent image build` that ends between the two renames of its install no longer loses the previous base image. The agent at start, the macOS and Windows capability checks and base-image boots restore `images/<os>.old` when `images/<os>` is missing or incomplete, a valid `images/<os>` wins over a leftover backup, an unfinished `images/<os>.build` is never used, and the next install keeps the backup until the new image is in place.
