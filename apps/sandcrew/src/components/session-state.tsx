// SPDX-License-Identifier: Apache-2.0
import { SessionState } from "@delino/sandcrew-protos/v1/common_pb";
import type { Session } from "@delino/sandcrew-protos/v1/session_pb";
import { BadgeVariant } from "delino-ui/badge";
import { formatTime, labels } from "../format.ts";
import { StateBadge } from "./ui.tsx";

const ACTIVE: ReadonlySet<SessionState> = new Set([
  SessionState.RUNNING,
  SessionState.STARTING,
  SessionState.RESUMING,
]);

/** The badge color of a session state; the text always says the state. */
export function sessionStateTone(state: SessionState): BadgeVariant {
  if (state === SessionState.WAITING_FOR_INPUT) return BadgeVariant.Accent;
  if (state === SessionState.DISCONNECTED) return BadgeVariant.Danger;
  if (ACTIVE.has(state)) return BadgeVariant.Accent;
  return BadgeVariant.Neutral;
}

/** The state of a session, with the end reason of an ended session. */
export function sessionStateText(session: Session): string {
  if (session.state === SessionState.ENDED) {
    return `Ended: ${labels.endReason[session.endReason]}`;
  }
  return labels.sessionState[session.state] ?? "Unknown";
}

export function SessionStateBadge({ session }: { session: Session }) {
  return (
    <StateBadge
      text={sessionStateText(session)}
      tone={sessionStateTone(session.state)}
    />
  );
}

/**
 * The scheduled deletion time of a paused session, when the
 * paused-session retention is limited.
 */
export function scheduledDeletionText(session: Session): string | undefined {
  if (
    session.state !== SessionState.PAUSED ||
    session.scheduledDeletionTime === undefined
  ) {
    return undefined;
  }
  return `Deleted on ${formatTime(session.scheduledDeletionTime)} unless it resumes`;
}
