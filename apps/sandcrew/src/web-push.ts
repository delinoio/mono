// SPDX-License-Identifier: Apache-2.0

/** The service worker at the site root. It handles Web Push only. */
export const SERVICE_WORKER_URL = "/sw.js";

/** What this browser supports and allows for Web Push. */
export enum PushSupport {
  /** The browser has no service worker, Push API or Notification API. */
  Unsupported = "unsupported",
  /** The user blocked notifications for the site in the browser. */
  Denied = "denied",
  /** The browser can ask for permission or already granted it. */
  Available = "available",
}

/** The browser APIs that the opt-in uses. Tests pass a fake. */
export interface PushEnvironment {
  serviceWorker: Pick<ServiceWorkerContainer, "register"> | undefined;
  notification:
    | {
        permission: NotificationPermission;
        requestPermission(): Promise<NotificationPermission>;
      }
    | undefined;
  hasPushManager: boolean;
}

export function browserPushEnvironment(): PushEnvironment {
  const nav = typeof navigator === "undefined" ? undefined : navigator;
  return {
    serviceWorker:
      nav !== undefined && "serviceWorker" in nav
        ? nav.serviceWorker
        : undefined,
    notification:
      typeof Notification === "undefined" ? undefined : Notification,
    hasPushManager: typeof PushManager !== "undefined",
  };
}

export function pushSupport(env: PushEnvironment): PushSupport {
  if (
    env.serviceWorker === undefined ||
    env.notification === undefined ||
    !env.hasPushManager
  ) {
    return PushSupport.Unsupported;
  }
  return env.notification.permission === "denied"
    ? PushSupport.Denied
    : PushSupport.Available;
}

/** Why turning on notifications did not complete. */
export enum PushOptInFailure {
  Unsupported = "unsupported",
  PermissionNotGranted = "permission-not-granted",
}

export class PushOptInError extends Error {
  constructor(readonly failure: PushOptInFailure) {
    super(
      failure === PushOptInFailure.Unsupported
        ? "This browser does not support notifications."
        : "Notifications are not allowed for this site. Allow them in the browser settings and try again.",
    );
  }
}

/** The subscription values that RegisterPushSubscription takes. */
export interface PushSubscriptionKeys {
  endpoint: string;
  p256dhKey: string;
  authKey: string;
}

/** Decodes a base64url string (with or without padding) into bytes. */
export function base64UrlToBytes(value: string): Uint8Array<ArrayBuffer> {
  const base64 = value.replaceAll("-", "+").replaceAll("_", "/");
  const padded = base64 + "=".repeat((4 - (base64.length % 4)) % 4);
  const binary = atob(padded);
  const bytes = new Uint8Array(new ArrayBuffer(binary.length));
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

/** Encodes bytes as base64url without padding. */
export function bytesToBase64Url(bytes: ArrayBuffer | null): string {
  if (bytes === null) return "";
  let binary = "";
  for (const byte of new Uint8Array(bytes)) binary += String.fromCharCode(byte);
  return btoa(binary)
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replace(/=+$/, "");
}

/** Waits until the registration has an active service worker. */
export async function activeWorker(
  registration: Pick<
    ServiceWorkerRegistration,
    "active" | "installing" | "waiting"
  >,
): Promise<void> {
  if (registration.active) return;
  const worker = registration.installing ?? registration.waiting;
  // Without a worker there is nothing to wait for; subscribe() reports it.
  if (!worker) return;
  await new Promise<void>((resolve, reject) => {
    const check = () => {
      if (worker.state === "activated") {
        worker.removeEventListener("statechange", check);
        resolve();
      } else if (worker.state === "redundant") {
        worker.removeEventListener("statechange", check);
        reject(new Error("The notification service worker did not start."));
      }
    };
    worker.addEventListener("statechange", check);
    check();
  });
}

/**
 * Turns on Web Push for this browser after the user clicked the button:
 * asks for the browser permission, loads the VAPID public key, registers
 * the service worker and subscribes. The browser permission prompt
 * appears only here, never on page load.
 */
export async function subscribeToPush(
  loadVapidPublicKey: () => Promise<string>,
  env: PushEnvironment = browserPushEnvironment(),
): Promise<PushSubscriptionKeys> {
  if (pushSupport(env) === PushSupport.Unsupported) {
    throw new PushOptInError(PushOptInFailure.Unsupported);
  }
  // Ask before any network wait: Safari shows the permission prompt only
  // during the click's transient activation, which a slow request can use
  // up. So the VAPID key loads after the answer.
  const permission = await env.notification!.requestPermission();
  if (permission !== "granted") {
    throw new PushOptInError(PushOptInFailure.PermissionNotGranted);
  }
  const vapidPublicKey = await loadVapidPublicKey();
  const registration = await env.serviceWorker!.register(SERVICE_WORKER_URL, {
    scope: "/",
  });
  // On the first opt-in, register() can resolve while the worker is still
  // installing, and subscribe() needs an active worker.
  await activeWorker(registration);
  const subscription = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: base64UrlToBytes(vapidPublicKey),
  });
  return {
    endpoint: subscription.endpoint,
    p256dhKey: bytesToBase64Url(subscription.getKey("p256dh")),
    authKey: bytesToBase64Url(subscription.getKey("auth")),
  };
}

/** A short browser name for the subscription list, for example "Firefox on Linux". */
export function browserLabel(userAgent: string): string {
  const browser = /Edg\//.test(userAgent)
    ? "Edge"
    : /Firefox\//.test(userAgent)
      ? "Firefox"
      : /Chrome\//.test(userAgent)
        ? "Chrome"
        : /Safari\//.test(userAgent)
          ? "Safari"
          : "Browser";
  const os = /Windows/.test(userAgent)
    ? "Windows"
    : /Android/.test(userAgent)
      ? "Android"
      : /iPhone|iPad/.test(userAgent)
        ? "iOS"
        : /Mac OS X|Macintosh/.test(userAgent)
          ? "macOS"
          : /Linux/.test(userAgent)
            ? "Linux"
            : undefined;
  return os === undefined ? browser : `${browser} on ${os}`;
}
