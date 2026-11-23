---
"ownsand-console": patch
---

After a confirmed action removes the button that opened a dialog (removing a member, revoking an invitation or an API key, transferring ownership) or navigates to another page (deleting or creating an organization), the keyboard focus now moves to the page heading instead of falling back to the page body. A dialog that closes with Escape or Cancel still returns the focus to its button. Page headings are focus targets only and are not in the Tab order. The focus is never taken from an element that already holds it, and the first render does not move it.
