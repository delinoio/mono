// SPDX-License-Identifier: Apache-2.0
import { useEffect, useState } from "react";
import { type UtcMonth, utcMonthOf } from "./format.ts";

/**
 * The longest delay that `setTimeout` accepts. A larger delay overflows a
 * signed 32-bit integer and fires at once, and a UTC month lasts up to about
 * 2.68e9 ms (more than the limit of about 2.15e9 ms), so the wait for the next
 * month is split into steps of at most this length.
 */
export const MAX_TIMER_DELAY_MS = 2 ** 31 - 1;

/** Milliseconds from `now` to the first instant of the next UTC month. */
function untilNextUtcMonth(now: Date): number {
  const next = Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 1);
  return next - now.getTime();
}

/**
 * The current UTC calendar month. It updates without a reload when the UTC
 * month changes while the component is mounted: a timer fires at the start of
 * the next month, and a return to the tab (`visibilitychange` to visible)
 * reads the clock at once, because a hidden tab may delay its timers. The
 * state changes only when the month changes, so a re-evaluation that finds the
 * same month does not render.
 */
export function useCurrentUtcMonth(): UtcMonth {
  const [current, setCurrent] = useState(() => utcMonthOf(new Date()));
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const refresh = () => {
      clearTimeout(timer);
      const now = new Date();
      const month = utcMonthOf(now);
      setCurrent((previous) =>
        previous.year === month.year && previous.month === month.month
          ? previous
          : month,
      );
      // At least 1 ms, so that an early timer cannot loop without waiting.
      const delay = Math.min(
        Math.max(untilNextUtcMonth(now), 1),
        MAX_TIMER_DELAY_MS,
      );
      timer = setTimeout(refresh, delay);
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") refresh();
    };
    // Also covers a clock that moved between the render and this effect.
    refresh();
    window.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      clearTimeout(timer);
      window.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, []);
  return current;
}
