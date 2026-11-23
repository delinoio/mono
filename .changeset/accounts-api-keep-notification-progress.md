---
"accounts-api": patch
---

Keep the delivery progress of a billing notification when the billing worker run times out or stops. Before, the worker sent the emails of a notification in one transaction, so an expired run context rolled back the record of every recipient that had already received the email, and the same notification stayed first in the queue. Now the worker claims a notification with a 5 minute lease, records each recipient right after its send, and defers an interrupted notification by 1 minute without counting a failed attempt, so the next attempt sends only to the recipients that are not recorded and a newer notification can run first.
