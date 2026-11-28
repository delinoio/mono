# servers-sandcrew-api foundation contract

## Scope

This contract owns the `sandcrew-api` control plane of [`sandcrew`](project-sandcrew.md) at `servers/sandcrew-api/` (`planned`). The control plane is the system of record for SandCrew organization settings, vendor credentials, session secrets, sessions, transcripts and Web Push subscriptions. It creates, pauses, resumes and deletes the SandRun dev session of each session, installs and starts the [bridge](cmds-sandcrew-bridge-foundation.md) in it, collects the bridge events, stores them as transcripts and serves them to the [console](apps-sandcrew-console-foundation.md).

The control plane runs no agent and no customer code. It is licensed under `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md).

Layout, following the `sandrun-api` server conventions:

- `servers/sandcrew-api/cmd/sandcrew-api/`: process entry point.
- `servers/sandcrew-api/internal/`: `config`, `server` (HTTP handler, `GET /healthz` and CORS), `api` (Connect handlers, authentication, authorization and rate limits), `auth` (Logto access tokens), `membership` (the SandRun membership cache and the reconciliation job), `sandrun` (the delegated SandRun client), `secrets` (encryption), `sessions` (the session state machine, pause and retention timers), `collector` (exec streams, guest log recovery and the tail), `transcripts` (R2 chunks), `bridgedist` (the embedded bridge binaries), `notify` (Web Push), `analytics` (PostHog), `lease`, `jobs` and `db` (Postgres pool and migrations).
- `servers/sandcrew-api/package.json`, `turbo.json`, `.env.example` and `Dockerfile`: workspace wiring, the `deploy` field and the container image.

## Runtime and Language

- Language: Go in the root module `github.com/delinoio/mono`. Import path: `github.com/delinoio/mono/servers/sandcrew-api/...`. Logging: `log/slog` with the OpenTelemetry log bridge.
- Runtime: one long-running process that serves Connect handlers on `ADDR` and metrics in the Prometheus format on `METRICS_ADDR`.
- Replicas and lease: like `sandrun-api`, the service runs as one replica, and a deployment briefly runs the new container next to the old one. Only the instance that holds the lease row in Postgres runs the background components: the collector, the pause, resume and retention timers, the transcript flusher, the membership reconciliation job, the notification sender, the pending deletion retries and the daily re-encryption job. Any instance serves the public RPCs, including the streams.
- Deployment: a Docker container on the shared DigitalOcean Droplet through the [deployment workflow](repository-workflow-contract.md#deployment-workflow) with the `deploy` target `droplet`, the service `sandcrew-api`, the host name `sandcrew.api.delino.io` and the container port `8080`. Releases use changesets. Database migrations stay compatible with the previous release during a deployment and a rollback (add columns and tables first, remove them in a later release).
- Container image: the image embeds the bridge binaries of the release targets (`linux-amd64`, `linux-arm64`, `windows-amd64`, `windows-arm64` and `darwin-arm64`), built from `cmds/sandcrew-bridge/` of the same commit with its `build:binaries` script, with their SHA-256 digests.
- Database: PlanetScale Postgres. Migration tool: goose, like `sandrun-api`.
- Object storage: one Cloudflare R2 bucket for transcript chunks.
- Development ports (recorded when the path is created): `127.0.0.1:18700` for HTTP and `127.0.0.1:19700` for metrics.

### Configuration

The server reads these environment variables without a service prefix, with the same parsing rules as `sandrun-api`. A value that fails validation stops the server at startup.

| Variable                    | Required | Default                          | Rule                                                                                     |
| --------------------------- | -------- | -------------------------------- | ---------------------------------------------------------------------------------------- |
| `ADDR`                      | no       | `127.0.0.1:18700`                | `host:port` of the HTTP/1.1 and h2c listener.                                            |
| `METRICS_ADDR`              | no       | empty (listener off)             | `host:port` of the metrics listener.                                                     |
| `LOG_LEVEL`                 | no       | `info`                           | `debug`, `info`, `warn` or `error`.                                                      |
| `DATABASE_URL`              | yes      |                                  | Postgres connection URL.                                                                 |
| `RUN_MIGRATIONS`            | no       | `true`                           | Apply the embedded migrations at startup.                                                |
| `LOGTO_ISSUER`              | yes      |                                  | Logto issuer, `<tenant endpoint>/oidc`.                                                  |
| `LOGTO_JWKS_URL`            | no       | `LOGTO_ISSUER` + `/jwks`         | Key set URL.                                                                             |
| `LOGTO_AUDIENCE`            | no       | `https://sandcrew.api.delino.io` | Expected `aud`.                                                                          |
| `LOGTO_TOKEN_ENDPOINT`      | yes      |                                  | Logto token endpoint for the machine-to-machine client credentials grant.                |
| `SANDRUN_M2M_CLIENT_ID`     | yes      |                                  | Machine-to-machine application ID for `https://sandrun.api.delino.io`.                   |
| `SANDRUN_M2M_CLIENT_SECRET` | yes      |                                  | Its secret. Service secret.                                                              |
| `SANDRUN_API_ORIGIN`        | no       | `https://sandrun.api.delino.io`  | Origin of the SandRun API.                                                               |
| `CORS_ORIGINS`              | no       | `https://sandcrew.delino.io`     | Comma-separated origins, with the SandRun format rules.                                  |
| `SANDCREW_SECRET_KEYS`      | yes      |                                  | Service secret; see [Credentials and secrets](#credentials-and-secrets).                 |
| `R2_ENDPOINT`               | yes      |                                  | R2 S3 account endpoint.                                                                  |
| `R2_BUCKET`                 | yes      |                                  | Bucket of transcript chunks.                                                             |
| `R2_ACCESS_KEY_ID`          | yes      |                                  | R2 access key ID. Service secret.                                                        |
| `R2_SECRET_ACCESS_KEY`      | yes      |                                  | R2 secret access key. Service secret.                                                    |
| `VAPID_PUBLIC_KEY`          | no       | empty (Web Push off)             | VAPID public key (P-256, base64url). Set the three `VAPID_*` variables together or none. |
| `VAPID_PRIVATE_KEY`         | no       | empty                            | VAPID private key. Service secret.                                                       |
| `VAPID_SUBJECT`             | no       | empty                            | VAPID subject (`mailto:` or `https:` URL of the operators).                              |
| `POSTHOG_API_KEY`           | no       | empty (analytics off)            | PostHog project API key.                                                                 |
| `POSTHOG_HOST`              | no       | `https://us.i.posthog.com`       | PostHog ingestion host.                                                                  |
| `OTEL_*`                    | no       | empty (no OTLP export)           | Standard OpenTelemetry variables, read through the shared telemetry package.             |

## Users and Operators

- Members of SandRun organizations, through the [console](apps-sandcrew-console-foundation.md).
- `sandrun-api`, which `sandcrew-api` calls with its machine-to-machine token for the acting user.
- The bridge in each session sandbox, reached only through SandRun dev session RPCs.
- Web Push services of the browsers, PostHog, Cloudflare R2 and Logto.
- Operators who deploy and monitor the service.

## Interfaces and Contracts

- Business APIs: the public services of the [protos contract](protos-sandcrew-api-contract.md).
- Operational endpoints: `GET /healthz` without authentication, and `GET /metrics` on `METRICS_ADDR`.
- Browser access: CORS allows only `https://sandcrew.delino.io` in production and the console's loopback development origins locally.

### Authentication

- Every public RPC requires a Logto access token for the API resource `https://sandcrew.api.delino.io` with the scope `sandcrew:use`, validated according to the [authentication contract](repository-auth-contract.md#token-validation). The server rejects ID tokens and machine-to-machine tokens (a `sub` equal to `client_id`).
- Logto registration: the console is a single-page application that requests the resources `https://sandcrew.api.delino.io` (scope `sandcrew:use`). `sandcrew-api` has one machine-to-machine application with the scope `sandrun:act-for-user` of the resource `https://sandrun.api.delino.io`.
- Calls to SandRun: `sandcrew-api` gets a machine-to-machine access token with the client credentials grant, caches it until 60 seconds before its expiry, and sends it with the header `Delino-Acting-User: <Logto user ID>` on every SandRun call. The acting user is the caller of the public RPC, or the owner of the session for background work on that session. `sandcrew-api` calls SandRun for a user only after its own check of that user ([Authorization](#authorization)) or for work on a session that the user owns.

### Authorization

| Action                                                                                                      | `owner`       | `admin`       | `member`      |
| ----------------------------------------------------------------------------------------------------------- | ------------- | ------------- | ------------- |
| Read and change the SandCrew organization settings; set and delete organization vendor credentials          | yes           | yes           | no            |
| Read the organization settings, list runner profiles, list organization credentials (without values)        | yes           | yes           | yes           |
| Set, list and delete the caller's own vendor credentials and session secrets                                | yes           | yes           | yes           |
| Create a session; list sessions and read the metadata of every session of the organization                  | yes           | yes           | yes           |
| Read the transcript, send input, cancel, change mode or options, resume, restart from transcript and delete | session owner | session owner | session owner |

- "Session owner" is the user who created the session. No role reads another user's transcript or sends input to another user's session. An `owner` or `admin` can end another user's dev session in the SandRun console; the SandCrew session then ends with `EXECUTOR_ENDED` and its transcript stays with its owner.
- A user without a membership in the organization gets `not_found` on every RPC with that `organization_id`. A session of another organization, or another user's session on an owner-only RPC, also gets `not_found`.
- Rate limits follow the [SandRun values](servers-sandrun-api-foundation.md#security): in-memory token buckets, 120 calls per client address per minute with a burst of 60, and in addition 60 input RPCs (`SendMessage`, `AnswerPermission`, `CancelTurn`, `SetSessionMode`, `SetSessionConfigOption`) per user per minute and 10 `CreateSession` and `RestartSessionFromTranscript` calls per user per minute and 60 per organization per minute. A rejected call fails with `resource_exhausted`.

### Membership

- Each RPC with an `organization_id` reads the caller's membership and role from SandRun `OrganizationService.GetOrganization` (delegated, `caller_role`). The server caches the result per user and organization for 30 seconds, also a `not_found`. `OrganizationService.ListOrganizations` of SandCrew reads SandRun `ListOrganizations` for the caller without the cache.
- When SandRun is unavailable and the cache has no fresh entry, the RPC fails with `unavailable`. The server never grants access from an expired entry.
- Every SandRun call checks the membership again in SandRun, so a user who left loses the ability to change dev sessions at once, and SandCrew reads end within the cache lifetime.
- Reconciliation job (lease holder, every 15 minutes, and 1 minute after startup): for each pair of organization and user that has SandCrew data (a session, a user vendor credential or a session secret), and for each user recorded as `updated_by` of organization settings or organization credentials, the job reads the membership with the delegated `GetOrganization`. For a `not_found`, it deletes all data of that user in that organization, as [session deletion](#session-deletion) does for each session, without a SandRun call for dev sessions that SandRun already ended. When no checked user of an organization is a member any more (the organization was deleted, or every SandCrew user left), the job also deletes the organization settings and the organization vendor credentials. Each deletion writes a `membership_reconciled` log record with the organization ID, the user ID and the counts, never content.
- The job checks at most 20 pairs at the same time and continues after a failed pair; `unavailable` from SandRun keeps the data until the next run.

### Organization settings

- `pause_delay`: 1 minute to 24 hours, whole seconds. Default: 10 minutes.
- `paused_session_retention`: 1 to 365 days in whole days, or `unlimited`. Default: 30 days.
- `allowed_runner_profile_ids`: SandRun `DEV_SESSION` runner profile IDs of the organization. Default: empty, so a new organization has no allowed profile until an `owner` or `admin` selects one. The server checks each ID against SandRun `ListDevSessionProfiles` at update and rejects an unknown or not selectable profile with `invalid_argument`. A profile deleted in SandRun later stays in the list and is shown as missing.
- A profile is selectable when SandRun reports it without `max_ttl` (`ProfileIneligibility` `HAS_MAXIMUM_LIFETIME` otherwise), with network mode `OUTBOUND` (`NETWORK_NOT_OUTBOUND` otherwise) and with a guest OS that SandCrew supports. `WINDOWS` profiles get `GUEST_OS_UNSUPPORTED` until OwnSand supports the `OUTBOUND` network mode for Windows guests; the change that removes this rule updates this contract.
- A settings change applies to the next evaluation of each timer. It does not change a running pause or a scheduled deletion time that was already shown, except that a shorter retention can make a paused session expire earlier, and the console shows the new time.
- The settings, keyed by the SandRun organization ID, record `updated_by` and `update_time`.

### Credentials and secrets

- Vendor credentials: one credential per pair of scope and vendor (`ORGANIZATION` per organization, `USER` per organization and user). Kinds by vendor: `CLAUDE_CODE` accepts `API_KEY` and `CLAUDE_OAUTH_TOKEN`; `OPENAI_CODEX` accepts `API_KEY` and `CODEX_AUTH_JSON`. Another kind fails with `invalid_argument`. `CODEX_AUTH_JSON` must be a JSON object of at most 64 KiB; the server checks only that it parses.
- Selection at session start: the user credential of the vendor when one exists, otherwise the organization credential. Without either, `CreateSession` fails with `failed_precondition` and a message that tells the user to add a credential. The session records the ID of the selected credential and the `value_set_at` that it used.
- Session secrets: per organization and user, at most 100. A name matches `^[A-Za-z_][A-Za-z0-9_]{0,127}$`, is unique per user and organization, and must not start with `SANDCREW_` or equal a variable that the [bridge contract](cmds-sandcrew-bridge-foundation.md#vendors) reserves; a value is at most 32 KiB. `CreateSession` takes secret IDs of the caller only, and their names and values together must encode to at most 192 KiB, so that the bridge exec stays within the SandRun limit of 256 KiB. The session stores the selected IDs, not the values.
- Injection: SandCrew passes the selected secrets and the model credential to the bridge exec as environment values, except `CODEX_AUTH_JSON`, which SandCrew writes as a guest file before the exec ([bridge contract](cmds-sandcrew-bridge-foundation.md#start)). Each exec that SandCrew starts in a session, at start and after a resume when the bridge process must start again, reads the current stored values of the selected secrets again. A deleted secret is omitted, and the session continues.
- Codex `auth.json` updates: on a `credential_update` event, the collector reads the guest file `.sandcrew/codex-home/auth.json` of the [bridge contract](cmds-sandcrew-bridge-foundation.md#state-directory) with `ReadDevSessionFile`, checks that it is a JSON object of at most 64 KiB and replaces the stored value of the credential that the session uses, with `updated_by` `AGENT_REFRESH`. It does not replace the value when a user set the credential after the session started (a newer `value_set_at`), or when the credential was deleted. The event and the file content are never stored in the tail or a transcript and never logged; a failed update increments the metric `sandcrew_credential_update_failures_total` and logs the kind and the session ID.
- Encryption: every credential and secret value is encrypted with AES-256-GCM. Each value records its key ID. The associated data is the organization ID, the owner user ID (empty for organization credentials), the kind (`vendor_credential` or `session_secret`) and the entity ID, so a value copied to another row does not decrypt.
- `SANDCREW_SECRET_KEYS` uses the format and rotation rules of [`SANDRUN_SECRET_KEYS`](servers-sandrun-api-foundation.md#secrets): a comma-separated list of `<key ID>:<key>`, the first entry encrypts, every entry decrypts, and a daily job of the lease holder re-encrypts values of old keys and logs the count. A missing or invalid value stops the server at startup.
- The API is write-only: it returns names, kinds, scopes and the creation and update times, never values. The server decrypts a value only for the exec or file write that uses it.

### Session lifecycle

Creation:

- `CreateSession` checks the membership, that the runner profile is in `allowed_runner_profile_ids` and selectable, the vendor credential and the secret IDs. It then calls SandRun `CreateDevSession` for the caller with the label `sandcrew <session ID>` and stores the session in `QUEUED` with the dev session ID and the first message as a pending input, in one transaction. If the transaction fails, it deletes the dev session.
- A SandRun `resource_exhausted` for `max_dev_sessions_per_user` fails `CreateSession` with `resource_exhausted` and the message `You have reached the session limit of this organization. Delete a session or ask an administrator.`. Paused sessions count, because SandRun counts paused dev sessions. SandCrew has no session limit of its own.
- When the dev session is `RUNNING`, the lease holder selects the bridge release target from the profile's guest OS and the guest architecture (a short probe exec: `uname -m` on Linux and macOS, `cmd /c echo %PROCESSOR_ARCHITECTURE%` on Windows), writes the bridge binary and checks its size, writes the start configuration (`BridgeStartConfig` as protobuf JSON in `start.json`) and, for `CODEX_AUTH_JSON`, the credential file, and starts the bridge with `ExecDevSession` ([bridge contract](cmds-sandcrew-bridge-foundation.md#start)). The session is `STARTING` from dev session creation until the bridge `hello` event.
- The first message, and every input that arrives before the bridge accepts input, is kept in the `pending_inputs` table and written to the inbox after `hello`.

States:

| SandCrew state      | Condition                                                                                                                                                     |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `QUEUED`            | The dev session is `QUEUED`.                                                                                                                                  |
| `STARTING`          | The dev session is `STARTING`, or `RUNNING` before the bridge `hello` event.                                                                                  |
| `RUNNING`           | The bridge reported `AgentActivity` `STARTING` or `WORKING`, or an input is pending in the inbox.                                                             |
| `WAITING_FOR_INPUT` | The bridge reported `IDLE` (a turn ended) or `WAITING_FOR_PERMISSION` (in `ASK` mode), and no input is pending.                                               |
| `PAUSING`           | SandCrew asked SandRun to pause, and the dev session is not yet `PAUSED`.                                                                                     |
| `PAUSED`            | The dev session is `PAUSED`.                                                                                                                                  |
| `RESUME_QUEUED`     | The dev session is `RESUME_QUEUED` (the resume waits for capacity on the device).                                                                             |
| `RESUMING`          | The dev session is `RESUMING`, or `RUNNING` again before the collector caught up with the guest event log.                                                    |
| `DISCONNECTED`      | SandRun reports the device of the dev session as `OFFLINE`, in any non-terminal state after `STARTING`. The previous state returns when it is `ONLINE` again. |
| `ENDED`             | See the end reasons below.                                                                                                                                    |

End reasons:

| `SessionEndReason`         | Cause                                                                                                                                                                                                                                                          |
| -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `START_FAILED`             | The dev session failed with `RUNNER_START_FAILED` or `NO_ELIGIBLE_DEVICE`, the bridge could not be written or started, or the bridge reported `AGENT_INSTALL_FAILED`, `CHECKSUM_MISMATCH`, `AGENT_START_FAILED` or `CREDENTIAL_MISSING` before its first turn. |
| `QUEUE_EXPIRED`            | The dev session expired in the SandRun queue (`MAX_QUEUE_WAIT`).                                                                                                                                                                                               |
| `AGENT_EXITED`             | The bridge reported `agent_exited`, or the bridge exec ended, after the start.                                                                                                                                                                                 |
| `DEVICE_LOST`              | The dev session failed with `RUNNER_LOST` (the device was removed or lost the sandbox).                                                                                                                                                                        |
| `RESTORE_FAILED`           | The dev session failed with `RESTORE_FAILED` (OwnSand could not restore the saved state).                                                                                                                                                                      |
| `PAUSED_RETENTION_EXPIRED` | The session stayed `PAUSED` longer than the paused-session retention.                                                                                                                                                                                          |
| `BRIDGE_UNSUPPORTED`       | The bridge protocol major version of the session is not supported by this server.                                                                                                                                                                              |
| `EXECUTOR_ENDED`           | The dev session ended for another reason: deleted in SandRun by another user, `SESSION_TTL_EXPIRED`, `MEMBER_REMOVED`, `PROFILE_DELETED` or a cancel in SandRun.                                                                                               |

- When SandCrew ends a session itself (`AGENT_EXITED`, `PAUSED_RETENTION_EXPIRED`, `BRIDGE_UNSUPPORTED`, `START_FAILED` after the bridge started), it deletes the dev session. Every end flushes the tail to R2. The transcript stays readable.
- The session state follows the dev session state from the collector stream and from a poll of `GetDevSession` every 30 seconds for each non-terminal session (every 5 minutes for `PAUSED` sessions), on the lease holder.

### Pause and resume

- Pause: when a session stays `WAITING_FOR_INPUT` for the organization's `pause_delay`, the lease holder flushes the tail of the session to R2 and calls SandRun `PauseDevSession`. The session is `PAUSING` until SandRun reports `PAUSED`. Open console streams do not prevent the pause.
- An input that arrives in `PAUSING` is kept pending. SandCrew starts the resume when the dev session is `PAUSED`.
- Resume: `SendMessage` and `AnswerPermission` on a `PAUSING` or `PAUSED` session store the input as pending and call SandRun `ResumeDevSession`; `ResumeSession` calls it without input. The RPC returns after SandRun accepted the resume, with the session in `RESUME_QUEUED` or `RESUMING`. When the dev session is `RUNNING`, the collector attaches again, reads the guest event log from the stored offset and then writes the pending inputs to the inbox.
- A resume that waits longer than the profile's maximum queue wait returns the dev session to `PAUSED` ([SandRun contract](servers-sandrun-api-foundation.md#dev-session-pause-and-resume)). SandCrew shows `PAUSED` with the notice that the resume waited too long, keeps the pending inputs and starts no new resume by itself. A new input or `ResumeSession` starts it again.
- Paused-session retention: the scheduled deletion time is the time of the dev session pause plus the retention. The lease holder checks every minute and, at that time, deletes the dev session and ends the session with `PAUSED_RETENTION_EXPIRED`. Pending inputs are discarded. `unlimited` schedules no deletion.

### Input delivery

- Each input RPC creates a `BridgeInput` with a new `input_id` and stores it in `pending_inputs` in the same transaction as its idempotency record (`client_message_id`, kept 24 hours; a repeated ID returns the first result).
- The lease holder writes pending inputs of a session in creation order with `WriteDevSessionFile` into the inbox, one file per input, while the session is `RUNNING` or `WAITING_FOR_INPUT`. An input stays pending until the bridge reports `input_accepted` or `input_rejected` for its `input_id`. A write that fails is retried with the same file name, so the bridge sees each input at most once.
- `CancelTurn`, `SetSessionMode` and `SetSessionConfigOption` on a paused session fail with `failed_precondition`: they apply only to a running agent. `AnswerPermission` checks that the permission request is still pending in the transcript.

### Collector

- The lease holder keeps one `AttachDevSessionExec` stream of the bridge exec for each session whose dev session is `RUNNING`. It continues the stream after each SandRun rotation from the next stdout offset.
- It splits stdout into lines and parses each line as a `BridgeEvent`. It stores an event only when its `sequence` is the stored last sequence plus 1, ignores events with a lower sequence and handles `credential_update` without storing it. stderr is never stored; the collector counts its bytes only.
- Recovery: when the stream reports `output_truncated`, when the exec ended more than the SandRun retention ago, when a parsed sequence skips a number, or after a collector restart, the collector reads the guest event log ([bridge contract](cmds-sandcrew-bridge-foundation.md#event-log)) with `ReadDevSessionFile` from the stored `log_end_offset` and stores the missing events, then attaches again from the current end of stdout.
- Version check: on `hello`, the collector checks the protocol major version. An unsupported major ends the session with `BRIDGE_UNSUPPORTED`.
- Tail: every stored event goes to the Postgres table `transcript_tail` in the transaction that updates the session's last sequence, `log_end_offset` and state. The same transaction sends `NOTIFY sandcrew_events` with the session ID and the sequence. Every instance listens to the channel and serves `WatchSession` streams from the tail and from R2.

### Transcripts

- Flush: the lease holder moves the tail of a session to one R2 chunk at each `turn_ended`, when the tail of the session reaches 1 MiB of encoded events or 1,000 events, before a pause and at the end of a session. It uploads the chunk, then inserts the chunk row and deletes the flushed tail rows in one transaction. A failed upload keeps the tail and is retried with backoff (1 second to 1 minute); `sandcrew_r2_flush_failures_total` counts the failures.
- Chunk object key: `transcripts/<organization_id>/<session_id>/<first sequence, 20 digits>-<last sequence, 20 digits>.jsonl`. Each line is a `TranscriptEvent` in protobuf JSON.
- Reads (`ListSessionEvents`, `WatchSession`): chunks from R2 for sequences that were flushed, then the tail. The server reads R2 itself; no signed URL leaves the server.
- Retention: a transcript is kept while its session exists. It is deleted immediately and permanently only when the owner deletes the session, when the owner's membership in the organization ends, or when the SandRun organization is deleted (both through the [membership reconciliation](#membership)).
- Transcript file for a new session: `RestartSessionFromTranscript` renders the previous transcript as Markdown (user messages, agent messages, tool call titles with their status, plans, diffs as unified diffs and permission records; thoughts and raw tool input and output are omitted) and writes it to `.sandcrew/previous-session.md` in the new workspace before the bridge starts. The first prompt says that the file holds the previous session's transcript, gives its path and says that the workspace is otherwise empty, followed by the user's message. The new session records `previous_session_id`.

### Session deletion

- `DeleteSession` (owner only) deletes the dev session with SandRun `DeleteDevSession` (a `not_found` counts as done), then in one transaction deletes the session row, its pending inputs, its tail and its chunk rows and records the chunk object keys in `pending_object_deletions`. It then deletes the R2 objects before it answers. The lease holder retries failed object deletions until they succeed. From the commit on, no RPC returns the transcript.
- Deletion is permanent. SandCrew keeps no backup copy of a transcript outside R2 and Postgres; Postgres backups follow the PlanetScale defaults and expire with them.

### Notifications

- Web Push with VAPID. `RegisterPushSubscription` stores the subscription endpoint and keys of one browser for the caller; the console calls it only after the user turns on notifications in the settings and the browser grants permission. A subscription is per user, not per organization.
- Triggers for the session owner: the session changes to `WAITING_FOR_INPUT` after a `turn_ended` event; a `permission_request` arrives in `ASK` mode; the session ends with `DEVICE_LOST`, `RESTORE_FAILED`, `PAUSED_RETENTION_EXPIRED` or `BRIDGE_UNSUPPORTED`.
- Suppression: no notification is sent while the owner has an open `WatchSession` stream for that session. Each instance records its open streams in the table `session_viewers` with a heartbeat every 30 seconds, and the sender ignores rows older than 90 seconds. The console keeps the stream open only while its page is visible.
- Payload: a JSON object with `organization_id`, `session_id`, `label` (the session label, or `Untitled session`) and `reason`: `TURN_ENDED`, `PERMISSION_REQUESTED`, or the `SessionEndReason` value name without its prefix (`DEVICE_LOST`, `RESTORE_FAILED`, `PAUSED_RETENTION_EXPIRED` or `BRIDGE_UNSUPPORTED`). The console's service worker opens the chat view at `/orgs/<organization_id>/sessions/<session_id>` from it. It never contains prompts, agent output, tool output or file names.
- Delivery: the lease holder sends with a TTL of 24 hours. A `404` or `410` answer deletes the subscription. Other failures are retried 3 times with backoff and then dropped with a log record.
- Without the `VAPID_*` variables, `GetWebPushConfig` fails with `failed_precondition` and no notification is sent.

## Storage

Postgres tables (UUID v7 primary keys unless stated):

- `organization_settings` (key: SandRun organization ID): allowed runner profile IDs, pause delay, paused-session retention (null for unlimited), `updated_by`, `update_time`.
- `vendor_credentials`: organization ID, scope, owner user ID (null for organization scope), vendor, kind, ciphertext, key ID, `created_at`, `value_set_at`, `updated_by`, update source. Unique per organization, scope, owner and vendor.
- `session_secrets`: organization ID, owner user ID, name, ciphertext, key ID, `created_at`, `value_set_at`. Unique per organization, owner and name.
- `sessions`: organization ID, owner user ID, label, vendor, runner profile ID, guest OS, dev session ID, state, end reason, permission mode, selected credential ID and its `value_set_at`, previous session ID, bridge version, protocol version, agent version, last sequence, `log_end_offset`, bridge exec ID, stdout offset, the times of creation, start, the last pause, the scheduled deletion and the end, and the last agent options (modes and configuration options, without content).
- `session_secret_selections`: session ID and session secret ID.
- `pending_inputs`: input ID, session ID, encoded `BridgeInput`, `created_at`, `written_at`.
- `input_idempotency`: session ID, `client_message_id`, result, `created_at` (deleted after 24 hours).
- `transcript_tail`: session ID, sequence, encoded event, size. Primary key: session ID and sequence.
- `transcript_chunks`: session ID, first and last sequence, object key, size.
- `pending_object_deletions`: object key, `created_at`, attempts.
- `push_subscriptions`: user ID, endpoint, encrypted keys, user agent summary, `created_at`, `last_success_at`.
- `session_viewers`: instance ID, stream ID, session ID, user ID, heartbeat time.
- `lease`: the lease row, like `sandrun-api`.
- R2: one bucket with the chunk objects of [Transcripts](#transcripts). No other content is stored in R2.
- No table stores prompts or output outside `transcript_tail`, and no table stores a credential or secret value in plain text.

## Security

- Trust boundaries: the console and the bridge are untrusted. The bridge runs next to the agent and the user's code in the sandbox, so every event and every guest file that the collector reads is untrusted input: the collector limits a line to 4 MiB, rejects lines that do not parse, validates enums and never follows a guest path that the bridge names (it reads only the paths of the bridge contract).
- The bridge holds no SandCrew, SandRun or OwnSand credential. The only secrets in a sandbox are the session's model credential and its selected secrets.
- Owner-only reads: every transcript read, input RPC and session deletion checks that the caller is the session owner after the membership check.
- Delegated calls: `sandcrew-api` names only users that it authorized for the current request, or the owner of the session for background work on that session and for the cleanup of that user's data.
- The machine-to-machine client secret, `SANDCREW_SECRET_KEYS`, the R2 credentials and the VAPID private key are service secrets according to the [environment contract](repository-environment-contract.md).

## Logging

- Structured `log/slog` records with `organization_id`, `user_id`, `session_id`, `dev_session_id`, the bridge `exec_id`, `bridge_version`, `bridge_protocol_version`, `vendor`, `agent_version`, the event kind and sequence numbers.
- Log every session state change after its transaction commits with the previous and new state and the end reason, every SandRun call result with the RPC and the Connect code, every collector recovery with the offsets, every flush with the sequence range and size, and every reconciliation deletion with counts.
- Never log tokens, credentials, secrets, `auth.json`, prompts, agent messages, thoughts, tool input or output, diffs, file contents or Web Push keys.

### Metrics and alerts

Metrics in the Prometheus format, exported through OpenTelemetry to Grafana Cloud. Labels use enum identifiers only.

- `sandcrew_sessions` (gauge by `state` and `vendor`).
- `sandcrew_queue_wait_seconds` (histogram by `phase`: `CREATE` or `RESUME`).
- `sandcrew_time_to_first_event_seconds` (histogram by `vendor`): from `CreateSession` to the first agent event after `hello`.
- `sandcrew_collector_lag_events` (histogram, sampled for each collected session every 30 seconds): the highest sequence that the collector saw in the guest (stream or event log) minus the stored sequence. `sandcrew_collector_event_age_seconds` (histogram): from the event `time` to its storage. `sandcrew_collector_recoveries_total` by cause.
- `sandcrew_pause_failures_total`, `sandcrew_resume_failures_total` and `sandcrew_session_ends_total` by `reason`.
- `sandcrew_bridge_errors_total` by `kind`.
- `sandcrew_r2_flush_failures_total` and `sandcrew_credential_update_failures_total`.
- A SandCrew dashboard is committed as Grafana dashboard JSON with the implementation, like the [SandRun dashboard](servers-sandrun-api-foundation.md#metrics).
- Alerts on collector lag, the resume and restore failure rate, and R2 flush failures go to the operators' contact point. Threshold process: during the first 14 days of internal use, the operators record the daily maximum and the 99th percentile of each alerted metric; a change to this contract then records each threshold (initially twice the observed 99th percentile, rounded up) together with the committed alert rules. Until that change, the alert rules notify on any R2 flush failure that lasts 15 minutes and on any `RESTORE_FAILED` end.

### Product analytics

- `sandcrew-api` sends PostHog events server-side, with the Logto user ID (`sub`) as the distinct ID and the SandRun organization ID as the group `organization`.
- Events and properties: `session_created` (`vendor`, `guest_os`), `message_sent`, `turn_cancelled`, `mode_changed` (`permission_mode` only), `session_resumed`, `session_deleted`, `session_restarted_from_transcript` and `credential_registered` (`kind` and `scope` only).
- No event or property contains prompt, output or secret content. The console has no PostHog autocapture and no session recording.
- Without `POSTHOG_API_KEY`, the server sends no events.

## Build and Test

- Package scripts wrap the Go toolchain like `sandrun-api`. Before finishing a change, run `pnpm exec turbo run test --filter=./servers/sandcrew-api`, `go vet ./...` and the `gofmt` check from `servers/AGENTS.md`.
- Postgres-backed tests use `SANDCREW_TEST_DATABASE_URL` like the SandRun tests.
- Integration tests with fakes of SandRun (`DevSessionService` with a scripted guest, `OrganizationService` and the delegated authentication), R2, Logto and a Web Push service: owner-only access, delegated calls with the acting user, the membership cache and reconciliation deletion (also of organization data), collector restart from the last sequence and guest log recovery after truncation, the tail flush at turn end, at the size threshold, before a pause and at the end, the pause delay and the resume through the queue (also a resume that waits too long), retention expiry, the `DEVICE_LOST`, `RESTORE_FAILED`, `AGENT_EXITED` and `EXECUTOR_ENDED` endings, start from transcript, credential encryption and key rotation, the `auth.json` update and its skip rules, bridge protocol compatibility and `BRIDGE_UNSUPPORTED`, the SandRun limit error, the notification triggers and suppression, and the absence of content in logs, metrics and analytics events.
- Acceptance with real devices and real vendor accounts is recorded in pull requests and Linear issues and is not performed in the MVP.

## Dependencies and Integrations

- Upstream: [`sandrun-api`](servers-sandrun-api-foundation.md) through its public Connect API with the Go types of `protos/delino/sandrun/v1/`; Logto; PlanetScale Postgres; Cloudflare R2; the browsers' Web Push services; PostHog; Grafana Cloud.
- Embedded: the [bridge](cmds-sandcrew-bridge-foundation.md) binaries of the same commit.
- Downstream: the [console](apps-sandcrew-console-foundation.md).
- Runtime order: `sandcrew-api` needs the SandRun delegated authentication and dev session pause and resume in production before its first deployment.
- New dependencies (an R2 client, a Web Push library, a PostHog client) need owner approval and a [dependency security](repository-dependency-security-contract.md) review in the change that adds them.

## Change Triggers

- Update the [project index](project-sandcrew.md) when components, origins, the layering, states, end reasons or the executor mapping change.
- Update the [protos contract](protos-sandcrew-api-contract.md) with every RPC, message, event or enum change.
- Update the [bridge contract](cmds-sandcrew-bridge-foundation.md) when the start, the inbox, the event log or the credential files change.
- Update the [console contract](apps-sandcrew-console-foundation.md) when a surface of the console changes.
- Update the SandRun contracts first when SandCrew needs a new SandRun capability, and the [authentication contract](repository-auth-contract.md) when the delegated scope or its operations change.

## References

- [project-sandcrew.md](project-sandcrew.md)
- [servers-sandrun-api-foundation.md](servers-sandrun-api-foundation.md)
- [protos-sandrun-api-contract.md](protos-sandrun-api-contract.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [Web Push protocol (RFC 8030)](https://www.rfc-editor.org/rfc/rfc8030)
- [VAPID (RFC 8292)](https://www.rfc-editor.org/rfc/rfc8292)
