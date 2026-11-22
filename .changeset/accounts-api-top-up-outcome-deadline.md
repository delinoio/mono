---
"accounts-api": patch
---

A top-up now records the result of its Paddle charge under its own 10-second deadline that starts after the call returned. Before, the charge call and the outcome write shared one 20-second deadline, so a Paddle timeout left too little time for the billing account lock and the write: the unknown result was lost, a manual caller got a database error instead of the provider-unavailable answer, and an automatic top-up run stopped.
