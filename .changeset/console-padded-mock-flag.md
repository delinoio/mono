---
"ownsand-console": patch
---

Use one normalized decision for the mock mode, so a padded `PUBLIC_OWNSAND_MOCK_API` value such as `true` selects both the mock sign-in and the in-browser mock API instead of mock sign-in with the real API transport.
