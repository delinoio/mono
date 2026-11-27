import assert from "node:assert/strict";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";

const [browserName, driverUrl, appUrl, output] = process.argv.slice(2);
assert(["safari", "firefox"].includes(browserName));
assert(
  driverUrl && appUrl && output,
  "Supply browser, WebDriver URL, fixture URL, and output directory.",
);
await mkdir(output, { recursive: true });

async function command(path, method, body) {
  const response = await fetch(driverUrl + path, {
    method,
    ...(body === undefined
      ? {}
      : {
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        }),
  });
  const { value } = await response.json();
  if (!response.ok) {
    throw new Error(`${method} ${path}: ${value?.error}: ${value?.message}`);
  }
  return value;
}

const { sessionId, capabilities } = await command("/session", "POST", {
  capabilities: {
    alwaysMatch: {
      browserName,
      ...(browserName === "firefox"
        ? { "moz:firefoxOptions": { args: ["-headless"] } }
        : {}),
    },
  },
});
console.log(
  `${capabilities.browserName} ${capabilities.browserVersion} (${capabilities.platformName})`,
);
const session = (path, body, method = "POST") =>
  command(`/session/${sessionId}${path}`, method, body);
const run = (fn, ...args) =>
  session("/execute/sync", {
    script: `return (${fn})(...arguments)`,
    args,
  });

async function waitFor(read, accept, description, timeout = 6000) {
  const deadline = Date.now() + timeout;
  let value;
  do {
    value = await read();
    if (accept(value)) return value;
    await delay(60);
  } while (Date.now() < deadline);
  throw new Error(`${description}: ${JSON.stringify(value)}`);
}

const label = "Usage by service, organization and meter";
const regionSelector = ".table-wrap";
const previousSelector = '[aria-label="Previous month"]';

async function openUsage(width) {
  await session("/frame", { id: null });
  await session("/url", { url: appUrl });
  await run(
    (width, appUrl) => {
      // Safari can enforce a minimum outer window width.
      const frame = document.createElement("iframe");
      frame.title = "Accounts viewport";
      frame.style.cssText = `display:block;width:${width}px;height:844px;border:0`;
      frame.src = appUrl;
      document.body.style.margin = "0";
      document.body.append(frame);
      for (const element of Array.from(document.body.children)) {
        if (element !== frame) element.style.display = "none";
      }
    },
    width,
    appUrl,
  );
  await session("/frame", { id: 0 });
  await waitFor(
    () =>
      run(() =>
        Boolean(
          window.billingUsageFixture &&
          Array.from(document.querySelectorAll('[role="tab"]')).some(
            (tab) => tab.textContent === "Usage",
          ),
        ),
      ),
    Boolean,
    "The signed-in usage fixture must load",
  );
  await run(() => {
    window.billingUsageFixture.seedKeyboardScroll();
    Array.from(document.querySelectorAll('[role="tab"]'))
      .find((tab) => tab.textContent === "Usage")
      .click();
  });
  await waitFor(
    () =>
      run(() => Boolean(document.querySelector("table tbody td:last-child"))),
    Boolean,
    "The seeded usage row must render",
  );
  assert.equal(
    await run(() => window.innerWidth),
    width,
    "fixture viewport width",
  );
}

function metrics(selector) {
  const wrapper = document.querySelector(selector);
  const caption = wrapper.querySelector("caption");
  const cost = wrapper.querySelector("tbody td:last-child");
  const rect = cost.getBoundingClientRect();
  const left = wrapper.getBoundingClientRect().left + wrapper.clientLeft;
  const style = getComputedStyle(wrapper);
  const namedBy = document.getElementById(
    wrapper.getAttribute("aria-labelledby") ?? "",
  );
  return {
    namedByCaption: namedBy === caption,
    caption: caption.textContent.trim(),
    role: wrapper.getAttribute("role"),
    tabindex: wrapper.getAttribute("tabindex"),
    focused: document.activeElement === wrapper,
    ring: `${style.outlineStyle} ${style.outlineWidth}`,
    overflow: wrapper.scrollWidth - wrapper.clientWidth,
    scrollLeft: wrapper.scrollLeft,
    costInView:
      rect.left >= left - 1 && rect.right <= left + wrapper.clientWidth + 1,
    headers: Array.from(wrapper.querySelectorAll('th[scope="col"]'), (el) =>
      el.textContent.trim(),
    ),
    values: Array.from(wrapper.querySelectorAll("tbody td"), (el) =>
      el.textContent.trim(),
    ),
    total: document
      .querySelector('[aria-label="Usage total"]')
      .textContent.trim(),
  };
}
const read = () => run(metrics, regionSelector);
const activeLabel = () =>
  run(
    () =>
      document.activeElement.getAttribute("aria-label") ??
      document.activeElement.textContent.trim(),
  );

const tab = "\uE004";
const shift = "\uE008";
const left = "\uE012";
const right = "\uE014";
async function press(key, backwards = false) {
  const keyDown = (value) => ({ type: "keyDown", value });
  const keyUp = (value) => ({ type: "keyUp", value });
  await session("/actions", {
    actions: [
      {
        type: "key",
        id: "keyboard",
        actions: backwards
          ? [keyDown(shift), keyDown(key), keyUp(key), keyUp(shift)]
          : [keyDown(key), keyUp(key)],
      },
    ],
  });
}

async function screenshot(file) {
  const encoded = await session("/screenshot", undefined, "GET");
  await writeFile(join(output, file), Buffer.from(encoded, "base64"));
}

async function scrollUntil(key, accept, description) {
  for (let attempt = 0; attempt < 45; attempt++) {
    const view = await read();
    assert.equal(view.focused, true);
    if (accept(view)) return view;
    await press(key);
    await delay(120);
  }
  throw new Error(`${description}: ${JSON.stringify(await read())}`);
}

try {
  await session("/window/rect", { width: 1400, height: 1000 });
  await openUsage(360);

  await waitFor(
    () =>
      run(() =>
        Array.from(document.querySelectorAll("button")).some(
          (button) =>
            button.textContent.trim() === "Show older entries" &&
            !button.disabled,
        ),
      ),
    Boolean,
    "The ledger continuation button must be available as the next Tab stop",
  );
  await run(
    (selector) => document.querySelector(selector).focus(),
    previousSelector,
  );
  assert.equal(await activeLabel(), "Previous month");
  await press(tab);

  const initial = await read();
  assert.equal(
    initial.focused,
    true,
    "Tab from Previous month enters the scroll region",
  );
  assert.equal(initial.tabindex, "0");
  assert.equal(initial.role, "region");
  assert.equal(initial.namedByCaption, true);
  assert.equal(initial.caption, label);
  assert.equal(initial.ring, "solid 2px");
  assert.equal(initial.scrollLeft, 0);
  assert(initial.overflow > 0);
  assert.equal(initial.costInView, false);
  await screenshot("360-keyboard-focus.png");

  const cost = await scrollUntil(
    right,
    (view) => view.costInView,
    "ArrowRight reveals Cost",
  );
  assert(cost.scrollLeft > 0);
  await screenshot("360-keyboard-cost.png");
  await scrollUntil(
    left,
    (view) => view.scrollLeft === 0,
    "ArrowLeft returns to the start",
  );

  await press(tab);
  assert.equal(await activeLabel(), "Show older entries");
  await press(tab, true);
  assert.equal((await read()).focused, true);
  await press(tab, true);
  assert.equal(await activeLabel(), "Previous month");
  console.log(
    JSON.stringify({
      width: 360,
      overflow: initial.overflow,
      ring: initial.ring,
      costScrollLeft: cost.scrollLeft,
      tabEscape: "both directions",
    }),
  );

  await openUsage(1280);
  const desktop = await read();
  assert.equal(desktop.caption, label);
  assert.deepEqual(desktop.headers, [
    "Service",
    "Organization",
    "Meter",
    "Quantity",
    "Cost",
  ]);
  assert.deepEqual(desktop.values, [
    "OwnSand",
    "0193a1b2-0000-7000-8000-000000000001",
    "Relay egress",
    "1,048,576 bytes",
    "$0.004567",
  ]);
  assert.equal(desktop.total, "$0.004567");
  assert.equal(desktop.costInView, true);
  await screenshot("1280-native-table.png");

  await run(
    (selector) => document.querySelector(selector).click(),
    previousSelector,
  );
  await waitFor(
    () =>
      run(() => document.body.textContent.includes("No usage in this month")),
    Boolean,
    "The previous month must show the empty message",
  );
  assert.equal(
    await run((selector) => document.querySelector(selector), regionSelector),
    null,
  );
  await screenshot("1280-empty-month.png");
  console.log(
    JSON.stringify({
      width: 1280,
      layout: "native table",
      headers: desktop.headers,
      total: desktop.total,
      emptyMonthHasRegion: false,
    }),
  );
} finally {
  await session("", undefined, "DELETE");
}
