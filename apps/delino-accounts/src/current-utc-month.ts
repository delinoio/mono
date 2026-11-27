// SPDX-License-Identifier: Apache-2.0
import { useEffect, useState } from "react";
import { type UtcMonth, utcMonthOf } from "./format.ts";

const WALL_CLOCK_RECHECK_MS = 60_000;

/** Milliseconds from `now` to the first instant of the next UTC month. */
function untilNextUtcMonth(now: Date): number {
  const next = Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 1);
  return next - now.getTime();
}

/** Tracks the current UTC month while the tab is visible. */
export function useCurrentUtcMonth(): UtcMonth {
  const [current, setCurrent] = useState(() => utcMonthOf(new Date()));
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const refresh = () => {
      clearTimeout(timer);
      if (document.visibilityState !== "visible") return;

      const now = new Date();
      const month = utcMonthOf(now);
      setCurrent((previous) =>
        previous.year === month.year && previous.month === month.month
          ? previous
          : month,
      );
      // Limit the wait in case the wall clock jumps across the boundary.
      const delay = Math.min(
        Math.max(untilNextUtcMonth(now), 1),
        WALL_CLOCK_RECHECK_MS,
      );
      timer = setTimeout(refresh, delay);
    };
    // Also covers a clock that moved between the render and this effect.
    refresh();
    window.addEventListener("visibilitychange", refresh);
    return () => {
      clearTimeout(timer);
      window.removeEventListener("visibilitychange", refresh);
    };
  }, []);
  return current;
}
