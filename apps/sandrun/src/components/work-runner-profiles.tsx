// SPDX-License-Identifier: Apache-2.0
import { Select, SelectOption } from "delino-ui/select";
import { useMemo } from "react";
import { useI18n } from "../i18n.ts";
import { useRunnerProfiles } from "../runner-profiles.ts";
import { ErrorNotice, RefreshFailedNotice, showsFullError } from "./ui.tsx";

export function useWorkRunnerProfiles(organizationId: string) {
  const query = useRunnerProfiles(organizationId);
  const profiles = showsFullError(query) ? undefined : query.data;
  const profileNames = useMemo(
    () => profiles && new Map(profiles.map((p) => [p.runnerProfileId, p.name])),
    [profiles],
  );
  return { query, profileNames };
}

export function RunnerProfileNotice({
  query,
}: {
  query: ReturnType<typeof useRunnerProfiles>;
}) {
  const { t } = useI18n();
  if (showsFullError(query)) {
    return (
      <ErrorNotice
        title={t("profiles.loadFailed")}
        error={query.error}
        onRetry={() => void query.refetch()}
      />
    );
  }
  return query.error != null ? <RefreshFailedNotice query={query} /> : null;
}

const ALL_PROFILES = "all";

export function RunnerProfileFilter({
  profileNames,
  value,
  onValueChange,
}: {
  profileNames: ReadonlyMap<string, string> | undefined;
  value: string;
  onValueChange: (value: string) => void;
}) {
  const { t } = useI18n();
  const allLabel = t("profiles.filter.all");
  const items = useMemo(
    () => [
      { value: ALL_PROFILES, label: allLabel },
      ...Array.from(profileNames ?? [], ([value, label]) => ({ value, label })),
    ],
    [allLabel, profileNames],
  );
  return (
    <Select
      label={t("profiles.filter.label")}
      disabled={profileNames === undefined}
      value={value || ALL_PROFILES}
      items={items}
      onValueChange={(value) =>
        onValueChange(value === ALL_PROFILES ? "" : (value ?? ""))
      }
    >
      {items.map(({ value, label }) => (
        <SelectOption key={value} value={value}>
          {label}
        </SelectOption>
      ))}
    </Select>
  );
}
