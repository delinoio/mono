// SPDX-License-Identifier: Apache-2.0
import { SessionState } from "@delino/sandcrew-protos/v1/common_pb";
import type { Session } from "@delino/sandcrew-protos/v1/session_pb";
import { BadgeVariant } from "delino-ui/badge";
import { formatTime, labels } from "../format.ts";
import { type Localizer, type Translate, useI18n } from "../i18n.ts";
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
export function sessionStateText(session: Session, t: Translate): string {
  if (session.state === SessionState.ENDED) {
    return t("session.endedWithReason", {
      reason: t(labels.endReason[session.endReason] ?? "enum.unknown"),
    });
  }
  return t(labels.sessionState[session.state] ?? "enum.unknown");
}

export function SessionStateBadge({ session }: { session: Session }) {
  const { t } = useI18n();
  return (
    <StateBadge
      text={sessionStateText(session, t)}
      tone={sessionStateTone(session.state)}
    />
  );
}

/**
 * The scheduled deletion time of a paused session, when the
 * paused-session retention is limited.
 */
export function scheduledDeletionText(
  session: Session,
  { lang, t }: Localizer,
): string | undefined {
  if (
    session.state !== SessionState.PAUSED ||
    session.scheduledDeletionTime === undefined
  ) {
    return undefined;
  }
  return t("session.scheduledDeletion", {
    time: formatTime(session.scheduledDeletionTime, lang),
  });
}
