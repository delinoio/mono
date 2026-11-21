// SPDX-License-Identifier: Apache-2.0
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { encodeMessages, FrameDecoder } from "../codec.ts";
import {
  type DataFrame,
  DataFrameKind,
  DataFrameSchema,
} from "../gen/delino/ownsand/v1/data_frame_pb.ts";
import type {
  DataChannelLike,
  PeerConnectionConfig,
  PeerConnectionConstructor,
  PeerConnectionLike,
  SessionDescriptionLike,
} from "../p2p/webrtc.ts";
import type { VirtualClock } from "./virtual-clock.ts";

type Listener = (event: unknown) => void;

class Emitter {
  readonly #listeners = new Map<string, Set<Listener>>();

  addEventListener(type: string, listener: Listener): void {
    let set = this.#listeners.get(type);
    if (!set) {
      set = new Set();
      this.#listeners.set(type, set);
    }
    set.add(listener);
  }

  removeEventListener(type: string, listener: Listener): void {
    this.#listeners.get(type)?.delete(listener);
  }

  emit(type: string, event: unknown = {}): void {
    for (const listener of [...(this.#listeners.get(type) ?? [])]) {
      listener(event);
    }
  }
}

/** The fake agent side of a data channel. */
export interface FakeAgentPeer {
  /** Sends a frame to the SDK. */
  send(frame: MessageInitShape<typeof DataFrameSchema>): void;
  /** Sends a session-end frame and closes the channel. */
  endSession(reason: number): void;
  /** Closes the channel without a session-end frame. */
  fail(): void;
}

export type FakeAgent = (frame: DataFrame, peer: FakeAgentPeer) => void;

export interface FakeWebRtcOptions {
  /** Never open the data channel (an ICE failure). */
  neverOpen?: boolean;
}

/** Test control over the send buffer of a fake data channel. */
export interface FakeChannelControl {
  /** The reported send buffer size; a test sets it to model back-pressure. */
  bufferedAmount: number;
  /** Runs after each message that the SDK sent, before the next one. */
  afterSend: (bytes: Uint8Array) => void;
  /** Sets `bufferedAmount` and emits `bufferedamountlow`, as a drained buffer. */
  drain(bufferedAmount?: number): void;
}

/** Records of every fake peer connection. */
export interface FakeWebRtc {
  RTCPeerConnection: PeerConnectionConstructor;
  connections: {
    config: PeerConnectionConfig;
    label: string;
    ordered: boolean;
  }[];
  /** Frames that the SDK sent, in order. */
  frames: DataFrame[];
  peers: FakeAgentPeer[];
  /** One control per data channel, in creation order. */
  channels: FakeChannelControl[];
}

class FakeChannel
  extends Emitter
  implements DataChannelLike, FakeChannelControl
{
  readyState = "connecting";
  binaryType = "blob";
  bufferedAmount = 0;
  bufferedAmountLowThreshold = 0;
  readonly label: string;
  onSend: (bytes: Uint8Array) => void = () => {};
  afterSend: (bytes: Uint8Array) => void = () => {};

  constructor(label: string) {
    super();
    this.label = label;
  }

  send(data: Uint8Array): void {
    if (this.readyState !== "open") throw new Error("channel not open");
    this.onSend(data);
    this.afterSend(data);
  }

  drain(bufferedAmount = 0): void {
    this.bufferedAmount = bufferedAmount;
    this.emit("bufferedamountlow");
  }

  close(): void {
    if (this.readyState === "closed") return;
    this.readyState = "closed";
    queueMicrotask(() => this.emit("close"));
  }
}

/** Builds a fake WebRTC implementation that talks to a fake agent. */
export function fakeWebRtc(
  clock: VirtualClock,
  agent: FakeAgent,
  options: FakeWebRtcOptions = {},
): FakeWebRtc {
  const record: FakeWebRtc = {
    RTCPeerConnection: undefined as never,
    connections: [],
    frames: [],
    peers: [],
    channels: [],
  };
  class FakePeerConnection extends Emitter implements PeerConnectionLike {
    iceGatheringState = "new";
    localDescription: SessionDescriptionLike | null = null;
    channel: FakeChannel | undefined;
    readonly #config: PeerConnectionConfig;

    constructor(config: PeerConnectionConfig) {
      super();
      this.#config = config;
    }

    createDataChannel(
      label: string,
      init: { ordered: boolean },
    ): DataChannelLike {
      const channel = new FakeChannel(label);
      this.channel = channel;
      record.channels.push(channel);
      record.connections.push({
        config: this.#config,
        label,
        ordered: init.ordered,
      });
      const decoder = new FrameDecoder();
      const peer: FakeAgentPeer = {
        send: (frame) => {
          for (const message of encodeMessages(
            create(DataFrameSchema, frame),
          )) {
            const copy = message.slice().buffer;
            queueMicrotask(() => {
              if (channel.readyState === "open") {
                channel.emit("message", { data: copy });
              }
            });
          }
        },
        endSession: (reason) => {
          peer.send({
            streamId: 1n,
            kind: DataFrameKind.SESSION_END,
            payload: { case: "sessionEnd", value: { reason } },
          });
          queueMicrotask(() => queueMicrotask(() => channel.close()));
        },
        fail: () => channel.close(),
      };
      record.peers.push(peer);
      channel.onSend = (bytes) => {
        for (const frame of decoder.push(bytes)) {
          record.frames.push(frame);
          queueMicrotask(() => agent(frame, peer));
        }
      };
      return channel;
    }

    async createOffer(): Promise<SessionDescriptionLike> {
      return { type: "offer", sdp: "v=0 fake-offer" };
    }

    async setLocalDescription(d: SessionDescriptionLike): Promise<void> {
      this.localDescription = d;
      this.iceGatheringState = "gathering";
      void clock.sleep(100).then(() => {
        this.iceGatheringState = "complete";
        this.localDescription = {
          type: "offer",
          sdp: "v=0 fake-offer with candidates",
        };
        this.emit("icegatheringstatechange");
      });
    }

    async setRemoteDescription(): Promise<void> {
      if (options.neverOpen) return;
      void clock.sleep(50).then(() => {
        if (this.channel && this.channel.readyState === "connecting") {
          this.channel.readyState = "open";
          this.channel.emit("open");
        }
      });
    }

    close(): void {
      this.channel?.close();
    }
  }
  record.RTCPeerConnection = FakePeerConnection;
  return record;
}
