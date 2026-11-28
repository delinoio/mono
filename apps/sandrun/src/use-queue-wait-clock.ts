// SPDX-License-Identifier: Apache-2.0
import { useEffect, useState } from "react";

const CLOCK_TICK_MS = 15_000;

export function useQueueWaitClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), CLOCK_TICK_MS);
    return () => clearInterval(timer);
  }, []);
  return now;
}
