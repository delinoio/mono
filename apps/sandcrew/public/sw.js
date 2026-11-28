// SPDX-License-Identifier: Apache-2.0
//
// The SandCrew console service worker. It handles Web Push only and caches
// nothing (docs/apps-sandcrew-console-foundation.md). The push payload is
// the JSON object of docs/servers-sandcrew-api-foundation.md#notifications:
// organization_id, session_id, label and reason. It never holds prompts or
// agent output.

const REASONS = {
  TURN_ENDED: "The agent is waiting for your input.",
  PERMISSION_REQUESTED: "The agent asks for permission.",
  DEVICE_LOST: "The session ended: its device was lost.",
  RESTORE_FAILED: "The session ended: its saved state could not be restored.",
  PAUSED_RETENTION_EXPIRED:
    "The session ended: the paused-session retention ended.",
  BRIDGE_UNSUPPORTED:
    "The session ended: it uses an unsupported bridge version.",
};

function readPayload(event) {
  try {
    const data = event.data ? event.data.json() : {};
    return data !== null && typeof data === "object" ? data : {};
  } catch {
    return {};
  }
}

function sessionUrl(payload) {
  const org = payload.organization_id;
  const session = payload.session_id;
  if (typeof org !== "string" || typeof session !== "string") return "/";
  return `/orgs/${encodeURIComponent(org)}/sessions/${encodeURIComponent(session)}`;
}

self.addEventListener("push", (event) => {
  const payload = readPayload(event);
  const title =
    typeof payload.label === "string" && payload.label !== ""
      ? payload.label
      : "Untitled session";
  const body =
    REASONS[payload.reason] ?? "Your SandCrew session needs attention.";
  event.waitUntil(
    self.registration.showNotification(title, {
      body,
      tag:
        typeof payload.session_id === "string" ? payload.session_id : undefined,
      data: { url: sessionUrl(payload) },
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL(event.notification.data?.url ?? "/", self.location.origin)
    .href;
  event.waitUntil(
    self.clients
      .matchAll({ type: "window", includeUncontrolled: true })
      .then((windows) => {
        for (const client of windows) {
          if (client.url === url && "focus" in client) return client.focus();
        }
        return self.clients.openWindow(url);
      }),
  );
});
