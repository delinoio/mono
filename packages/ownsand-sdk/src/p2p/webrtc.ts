// SPDX-License-Identifier: Apache-2.0

/**
 * The subset of the W3C WebRTC API that the SDK uses. Node.js has no
 * built-in WebRTC implementation, so the SDK bundles none: it uses
 * `globalThis.RTCPeerConnection` when the runtime provides one, or a
 * constructor that the caller passes (for example from a W3C-compatible
 * WebRTC library). Without either, the SDK uses the relay only.
 */
export interface DataChannelLike {
  readonly label: string;
  readonly readyState: string;
  binaryType: string;
  readonly bufferedAmount: number;
  bufferedAmountLowThreshold: number;
  send(data: Uint8Array): void;
  close(): void;
  addEventListener(type: string, listener: (event: unknown) => void): void;
  removeEventListener(type: string, listener: (event: unknown) => void): void;
}

export interface SessionDescriptionLike {
  type: string;
  sdp?: string;
}

export interface PeerConnectionLike {
  createDataChannel(label: string, init: { ordered: boolean }): DataChannelLike;
  createOffer(): Promise<SessionDescriptionLike>;
  setLocalDescription(description: SessionDescriptionLike): Promise<void>;
  readonly localDescription: SessionDescriptionLike | null;
  readonly iceGatheringState: string;
  setRemoteDescription(description: SessionDescriptionLike): Promise<void>;
  addEventListener(type: string, listener: (event: unknown) => void): void;
  removeEventListener(type: string, listener: (event: unknown) => void): void;
  close(): void;
}

export interface PeerConnectionConfig {
  iceServers: { urls: string[] }[];
}

export type PeerConnectionConstructor = new (
  config: PeerConnectionConfig,
) => PeerConnectionLike;

/** Label of the one data channel of a session. */
export const DATA_CHANNEL_LABEL = "ownsand-data";

/** Returns the runtime's RTCPeerConnection, if it has one. */
export function runtimePeerConnection(): PeerConnectionConstructor | undefined {
  const ctor = (globalThis as { RTCPeerConnection?: unknown })
    .RTCPeerConnection;
  return typeof ctor === "function"
    ? (ctor as PeerConnectionConstructor)
    : undefined;
}
