---
"sandrun-api": patch
---

Keep expired runner records and their GitHub registration IDs while sandbox cleanup is pending. After cleanup clears the marker, the next daily retention pass can delete the runner using its original end time.
