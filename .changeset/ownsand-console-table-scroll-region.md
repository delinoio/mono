---
"ownsand-console": patch
---

The sandbox and device tables now sit in a named scroll region ("Sandbox list" and "Device list", `role="region"` with `tabindex="0"`). A keyboard user can focus the region with `Tab` and scroll a table that is wider than its column with the arrow keys; the region shows the same 2 px focus ring as a link for keyboard focus and no ring for a mouse click. The columns and the billing usage table are unchanged, and the loading, empty and error states render no region.
