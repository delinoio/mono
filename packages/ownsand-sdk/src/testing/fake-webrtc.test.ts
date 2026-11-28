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
    rtc,
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
    const { clock, rtc, connection, channel, opens } = connect({
      hold: ["createOffer"],
      openBeforeAnswer: true,
      openDelayMs: 0,
    });

    const offer = connection.createOffer();
    expect(rtc.held("createOffer")).toBe(1);
    expect(channel.readyState).toBe("connecting");
    expect(opens()).toBe(0);

    rtc.release("createOffer");
    await offer;
    expect(channel.readyState).toBe("open");
    expect(opens()).toBe(1);

    await connection.setRemoteDescription({ type: "answer" });
    await clock.advance(1_000);
    expect(channel.readyState).toBe("open");
    expect(opens()).toBe(1);
  });

  test("resolves a held offer without reopening a closed channel", async () => {
    const { clock, rtc, connection, channel, opens } = connect({
      hold: ["createOffer"],
      openBeforeAnswer: true,
    });

    const offer = connection.createOffer();
    expect(rtc.held("createOffer")).toBe(1);
    connection.close();
    expect(channel.readyState).toBe("closed");

    rtc.release("createOffer");
    expect(await offer).toEqual({ type: "offer", sdp: "v=0 fake-offer" });
    await clock.settle();
    expect(channel.readyState).toBe("closed");
    expect(opens()).toBe(0);
  });

  test("keeps the channel closed when it closes before the queued open runs", async () => {
    const { connection, channel, opens } = connect({ openBeforeAnswer: true });

    const offer = connection.createOffer();
    // Let createOffer queue the early open before closing the connection.
    await Promise.resolve();
    expect(channel.readyState).toBe("connecting");
    connection.close();

    await offer;
    expect(channel.readyState).toBe("closed");
    expect(opens()).toBe(0);
  });
});

describe("fakeWebRtc held methods", () => {
  test.each([
    "createOffer",
    "setLocalDescription",
    "setRemoteDescription",
  ] as const)("%s counts only calls waiting for release", async (method) => {
    const { clock, rtc, connection } = connect({
      hold: [method],
      openDelayMs: 0,
    });
    const calls = {
      createOffer: () => connection.createOffer(),
      setLocalDescription: () =>
        connection.setLocalDescription({ type: "offer", sdp: "v=0" }),
      setRemoteDescription: () =>
        connection.setRemoteDescription({ type: "answer" }),
    };
    let completed = 0;
    const invoke = async () => {
      await calls[method]();
      completed++;
    };
    const pending = [invoke(), invoke()];
    await clock.settle();
    expect(completed).toBe(0);
    expect(rtc.held(method)).toBe(2);

    rtc.release(method);
    expect(rtc.held(method)).toBe(0);
    await Promise.all(pending);
    expect(completed).toBe(2);

    const later = invoke();
    expect(rtc.held(method)).toBe(0);
    await later;
    expect(rtc.held(method)).toBe(0);

    rtc.release(method);
    const afterRepeatedRelease = invoke();
    expect(rtc.held(method)).toBe(0);
    await afterRepeatedRelease;
    expect(rtc.held(method)).toBe(0);
    expect(completed).toBe(4);
  });

  test("releases each configured method independently before or during calls", async () => {
    const { clock, rtc, connection, channel } = connect({
      hold: ["createOffer", "setLocalDescription"],
      openDelayMs: 0,
    });
    rtc.release("createOffer");
    const offer = connection.createOffer();
    expect(rtc.held("createOffer")).toBe(0);
    const description = await offer;
    expect(description).toEqual({ type: "offer", sdp: "v=0 fake-offer" });
    expect(rtc.held("createOffer")).toBe(0);

    const local = connection.setLocalDescription(description);
    await clock.settle();
    expect(rtc.held("setLocalDescription")).toBe(1);
    expect(connection.localDescription).toBeNull();

    const remote = connection.setRemoteDescription({ type: "answer" });
    expect(rtc.held("setRemoteDescription")).toBe(0);
    await remote;
    expect(rtc.held("setRemoteDescription")).toBe(0);
    expect(channel.readyState).toBe("open");
    expect(rtc.held("setLocalDescription")).toBe(1);

    rtc.release("setLocalDescription");
    await local;
    expect(rtc.held("setLocalDescription")).toBe(0);
    expect(connection.localDescription).toEqual(description);
  });
});
