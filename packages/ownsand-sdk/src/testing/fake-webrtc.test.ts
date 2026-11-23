// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { fakeWebRtc, type FakeWebRtcOptions } from "./fake-webrtc.ts";
import { VirtualClock } from "./virtual-clock.ts";

/** Creates a connection with one data channel and a log of its open events. */
function connect(options: FakeWebRtcOptions) {
  const clock = new VirtualClock();
  const rtc = fakeWebRtc(clock, () => {}, options);
  const connection = new rtc.RTCPeerConnection({ iceServers: [] });
  const channel = connection.createDataChannel("ownsand-data", {
    ordered: true,
  });
  let opened = 0;
  channel.addEventListener("open", () => {
    opened++;
  });
  return {
    clock,
    connection,
    channel,
    opens: () => opened,
  };
}

describe("fakeWebRtc open timing", () => {
  test("opens the channel before setRemoteDescription resolves when openDelayMs is zero", async () => {
    const { connection, channel, opens } = connect({ openDelayMs: 0 });

    await connection.setRemoteDescription({ type: "answer" });

    // No clock advance happened.
    expect(channel.readyState).toBe("open");
    expect(opens()).toBe(1);
  });

  test("opens through the virtual clock for a positive openDelayMs", async () => {
    const { clock, connection, channel, opens } = connect({
      openDelayMs: 200,
    });

    await connection.setRemoteDescription({ type: "answer" });
    expect(channel.readyState).toBe("connecting");
    expect(opens()).toBe(0);

    await clock.advance(199);
    expect(channel.readyState).toBe("connecting");
    await clock.advance(1);
    expect(channel.readyState).toBe("open");
    expect(opens()).toBe(1);
  });

  test("opens after the default 50 ms delay", async () => {
    const { clock, connection, channel, opens } = connect({});

    await connection.setRemoteDescription({ type: "answer" });
    expect(channel.readyState).toBe("connecting");

    await clock.advance(49);
    expect(channel.readyState).toBe("connecting");
    await clock.advance(1);
    expect(channel.readyState).toBe("open");
    expect(opens()).toBe(1);
  });

  test("never opens the channel when neverOpen is set, even with a zero delay", async () => {
    const { clock, connection, channel, opens } = connect({
      neverOpen: true,
      openDelayMs: 0,
    });

    await connection.setRemoteDescription({ type: "answer" });
    await clock.advance(60_000);
    expect(channel.readyState).toBe("connecting");
    expect(opens()).toBe(0);
  });

  test("keeps a channel that opened before the answer open exactly once", async () => {
    const { clock, connection, channel, opens } = connect({
      openBeforeAnswer: true,
      openDelayMs: 0,
    });

    await connection.createOffer();
    await clock.settle();
    expect(channel.readyState).toBe("open");

    await connection.setRemoteDescription({ type: "answer" });
    await clock.advance(1_000);
    expect(channel.readyState).toBe("open");
    expect(opens()).toBe(1);
  });
});
