---
ownsand-console: patch
---

The Device select of the Create sandbox dialog now shows the state of each device, for example `build-box-1 (Online)` and `studio-mac (Offline)`, in the list and in the trigger. When a create fails with `Unavailable` for a selected device that the loaded device list shows as offline, the dialog names that device and says that the sandbox cannot start on it, instead of the general "OwnSand is not available right now" text. Every other `Unavailable` error keeps the general text.
