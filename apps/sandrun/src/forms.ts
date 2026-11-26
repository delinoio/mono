// SPDX-License-Identifier: Apache-2.0

/**
 * Client-side checks of the console forms. They mirror the rules of the
 * control plane contract so that the user sees the problem before the
 * request; the control plane checks them again.
 */

import { create } from "@bufbuild/protobuf";
import {
  durationFromMs,
  type FieldMask,
  FieldMaskSchema,
} from "@bufbuild/protobuf/wkt";
import {
  AgentVendor,
  GuestOs,
  NetworkMode,
  Priority,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import {
  type RunnerProfile,
  RunnerProfileSchema,
} from "@delino/sandrun-protos/v1/runner_profile_pb";
import { durationMs } from "./format.ts";

export const MINUTE_MS = 60_000;
export const HOUR_MS = 60 * MINUTE_MS;
export const MAX_TTL_MS = 7 * 24 * HOUR_MS;
export const MAX_QUEUE_WAIT_MS = 24 * HOUR_MS;
export const OPENAI_MAX_QUEUE_WAIT_MS = 5 * MINUTE_MS;
export const MAX_WARM_COUNT = 10;

// Runner profile resource limits from servers/sandrun-api/internal/profile/validate.go.
export const MAX_VCPU_COUNT = 1024;
export const MAX_MEMORY_MIB = 16_777_216;
export const MAX_DISK_GIB = 1_048_576;

const PROFILE_NAME = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

/** How a runner profile form names its source. */
export enum SourceKind {
  BaseImage = "base_image",
  Template = "template",
}

/** The values of the runner profile form. */
export interface ProfileFormValues {
  name: string;
  workloadKind: WorkloadKind;
  agentIntegrationId: string;
  workspacePath: string;
  guestOs: GuestOs;
  sourceKind: SourceKind;
  templateId: string;
  vcpuCount: number | null;
  memoryMib: number | null;
  diskGib: number | null;
  idleTtlMinutes: number | null;
  maxTtlHours: number | null;
  maxQueueWaitMinutes: number | null;
  /** `UNSPECIFIED` selects the default of the workload kind. */
  priority: Priority;
  warmCount: number | null;
}

export type ProfileField = keyof ProfileFormValues;
export type ProfileErrors = Partial<Record<ProfileField, string>>;

export function emptyProfileForm(): ProfileFormValues {
  return {
    name: "",
    workloadKind: WorkloadKind.GITHUB_JOB,
    agentIntegrationId: "",
    workspacePath: "",
    guestOs: GuestOs.LINUX,
    sourceKind: SourceKind.BaseImage,
    templateId: "",
    vcpuCount: 2,
    memoryMib: 4096,
    diskGib: 32,
    idleTtlMinutes: 30,
    maxTtlHours: 6,
    maxQueueWaitMinutes: 60,
    priority: Priority.UNSPECIFIED,
    warmCount: 0,
  };
}

/** The guest OSes on which a vendor runs its runner program. */
export const VENDOR_GUEST_OSES: Readonly<
  Record<AgentVendor, readonly GuestOs[]>
> = {
  [AgentVendor.UNSPECIFIED]: [],
  [AgentVendor.CLAUDE_CODE]: [GuestOs.LINUX, GuestOs.MACOS],
  [AgentVendor.CURSOR]: [GuestOs.LINUX, GuestOs.MACOS, GuestOs.WINDOWS],
  [AgentVendor.OPENAI_CODEX]: [GuestOs.LINUX, GuestOs.MACOS, GuestOs.WINDOWS],
  [AgentVendor.DEVIN]: [GuestOs.LINUX, GuestOs.MACOS, GuestOs.WINDOWS],
};

/** Reports whether a path is absolute in the syntax of the guest OS. */
export function isAbsoluteGuestPath(guestOs: GuestOs, path: string): boolean {
  if (guestOs === GuestOs.WINDOWS) return /^[A-Za-z]:\\/.test(path);
  return path.startsWith("/");
}

function positive(value: number | null): boolean {
  return value !== null && Number.isFinite(value) && value > 0;
}

function normalizedDurationMs(value: number, unitMs: number): number {
  const ms = value * unitMs;
  const nearest = Math.round(ms);
  // Allow roundoff from displaying milliseconds in minutes or hours and
  // multiplying them back. Leave genuine fractions for field validation.
  return Math.abs(ms - nearest) <= Math.abs(ms) * Number.EPSILON ? nearest : ms;
}

const WHOLE_MILLISECONDS = "Enter a duration in whole milliseconds.";

/**
 * Returns the field errors of a runner profile form, or an empty object.
 * `vendor` is the vendor of the selected agent integration.
 */
export function profileErrors(
  values: ProfileFormValues,
  vendor: AgentVendor | undefined,
): ProfileErrors {
  const errors: ProfileErrors = {};
  if (
    values.name.length < 1 ||
    values.name.length > 40 ||
    !PROFILE_NAME.test(values.name)
  ) {
    errors.name =
      "Use 1 to 40 lowercase letters, digits and single hyphens, for example linux-build.";
  }
  if (
    values.workloadKind === WorkloadKind.AGENT_SESSION &&
    values.agentIntegrationId === ""
  ) {
    errors.agentIntegrationId = "Select the agent integration of the profile.";
  }
  if (values.workloadKind === WorkloadKind.DEV_SESSION) {
    if (values.workspacePath.trim() === "") {
      errors.workspacePath =
        "Enter the workspace path. Dev session sync writes into it.";
    } else if (!isAbsoluteGuestPath(values.guestOs, values.workspacePath)) {
      errors.workspacePath =
        values.guestOs === GuestOs.WINDOWS
          ? "Enter an absolute Windows path, for example C:\\work."
          : "Enter an absolute path, for example /home/dev/work.";
    }
  }
  if (
    values.workloadKind === WorkloadKind.AGENT_SESSION &&
    vendor !== undefined &&
    !VENDOR_GUEST_OSES[vendor].includes(values.guestOs)
  ) {
    errors.guestOs =
      "The vendor of this integration does not run on this guest OS.";
  }
  if (
    values.sourceKind === SourceKind.Template &&
    values.templateId.trim() === ""
  ) {
    errors.templateId = "Enter the ID of an OwnSand template of this guest OS.";
  }
  if (values.vcpuCount === null || values.vcpuCount <= 0) {
    errors.vcpuCount = "Enter at least 1 vCPU.";
  } else if (
    !Number.isInteger(values.vcpuCount) ||
    values.vcpuCount > MAX_VCPU_COUNT
  ) {
    errors.vcpuCount = `Enter a whole number of vCPUs from 1 to ${MAX_VCPU_COUNT}.`;
  }
  if (values.memoryMib === null || values.memoryMib <= 0) {
    errors.memoryMib = "Enter the memory in MiB.";
  } else if (
    !Number.isInteger(values.memoryMib) ||
    values.memoryMib > MAX_MEMORY_MIB
  ) {
    errors.memoryMib = `Enter a whole number of MiB from 1 to ${MAX_MEMORY_MIB}.`;
  }
  if (values.diskGib === null || values.diskGib <= 0) {
    errors.diskGib = "Enter the disk size in GiB.";
  } else if (
    !Number.isInteger(values.diskGib) ||
    values.diskGib > MAX_DISK_GIB
  ) {
    errors.diskGib = `Enter a whole number of GiB from 1 to ${MAX_DISK_GIB}.`;
  }
  const idleTtlMs = normalizedDurationMs(values.idleTtlMinutes ?? 0, MINUTE_MS);
  const maxTtlMs = normalizedDurationMs(values.maxTtlHours ?? 0, HOUR_MS);
  const maxQueueWaitMs = normalizedDurationMs(
    values.maxQueueWaitMinutes ?? 0,
    MINUTE_MS,
  );
  if (!positive(values.idleTtlMinutes)) {
    errors.idleTtlMinutes = "Enter an idle TTL greater than zero.";
  } else if (Number.isFinite(idleTtlMs) && !Number.isInteger(idleTtlMs)) {
    errors.idleTtlMinutes = WHOLE_MILLISECONDS;
  }
  if (!positive(values.maxTtlHours)) {
    errors.maxTtlHours = "Enter a maximum TTL greater than zero.";
  } else if (maxTtlMs > MAX_TTL_MS) {
    errors.maxTtlHours = "The maximum TTL is at most 7 days (168 hours).";
  } else if (!Number.isInteger(maxTtlMs)) {
    errors.maxTtlHours = WHOLE_MILLISECONDS;
  } else if (errors.idleTtlMinutes === undefined && idleTtlMs > maxTtlMs) {
    errors.idleTtlMinutes =
      "The idle TTL must not be longer than the maximum TTL.";
  }
  const limit =
    vendor === AgentVendor.OPENAI_CODEX &&
    values.workloadKind === WorkloadKind.AGENT_SESSION
      ? OPENAI_MAX_QUEUE_WAIT_MS
      : MAX_QUEUE_WAIT_MS;
  if (!positive(values.maxQueueWaitMinutes)) {
    errors.maxQueueWaitMinutes =
      "Enter a maximum queue wait greater than zero.";
  } else if (maxQueueWaitMs > limit) {
    errors.maxQueueWaitMinutes =
      limit === OPENAI_MAX_QUEUE_WAIT_MS
        ? "OpenAI waits at most 5 minutes for an executor."
        : "The maximum queue wait is at most 24 hours (1440 minutes).";
  } else if (!Number.isInteger(maxQueueWaitMs)) {
    errors.maxQueueWaitMinutes = WHOLE_MILLISECONDS;
  }
  if (
    values.warmCount === null ||
    !Number.isInteger(values.warmCount) ||
    values.warmCount < 0 ||
    values.warmCount > MAX_WARM_COUNT
  ) {
    errors.warmCount = "Enter a warm count from 0 to 10.";
  }
  return errors;
}

/** Builds the runner profile of a valid form. */
export function profileFromForm(values: ProfileFormValues): RunnerProfile {
  return create(RunnerProfileSchema, {
    name: values.name,
    workloadKind: values.workloadKind,
    agentIntegrationId:
      values.workloadKind === WorkloadKind.AGENT_SESSION
        ? values.agentIntegrationId
        : "",
    workspacePath:
      values.workloadKind === WorkloadKind.DEV_SESSION
        ? values.workspacePath
        : "",
    guestOs: values.guestOs,
    source:
      values.sourceKind === SourceKind.BaseImage
        ? { case: "baseImage", value: values.guestOs }
        : { case: "ownsandTemplateId", value: values.templateId.trim() },
    resources: {
      vcpuCount: values.vcpuCount!,
      memoryMib: BigInt(values.memoryMib!),
      diskGib: BigInt(values.diskGib!),
    },
    networkMode: NetworkMode.OUTBOUND,
    idleTtl: durationFromMs(
      normalizedDurationMs(values.idleTtlMinutes!, MINUTE_MS),
    ),
    maxTtl: durationFromMs(normalizedDurationMs(values.maxTtlHours!, HOUR_MS)),
    maxQueueWait: durationFromMs(
      normalizedDurationMs(values.maxQueueWaitMinutes!, MINUTE_MS),
    ),
    priority: values.priority,
    warmCount: values.warmCount!,
  });
}

/** The form values of an existing profile. */
export function formFromProfile(profile: RunnerProfile): ProfileFormValues {
  return {
    name: profile.name,
    workloadKind: profile.workloadKind,
    agentIntegrationId: profile.agentIntegrationId,
    workspacePath: profile.workspacePath,
    guestOs: profile.guestOs,
    sourceKind:
      profile.source.case === "ownsandTemplateId"
        ? SourceKind.Template
        : SourceKind.BaseImage,
    templateId:
      profile.source.case === "ownsandTemplateId" ? profile.source.value : "",
    vcpuCount: profile.resources?.vcpuCount ?? null,
    memoryMib:
      profile.resources === undefined
        ? null
        : Number(profile.resources.memoryMib),
    diskGib:
      profile.resources === undefined
        ? null
        : Number(profile.resources.diskGib),
    idleTtlMinutes: minutes(durationMs(profile.idleTtl)),
    maxTtlHours: hours(durationMs(profile.maxTtl)),
    maxQueueWaitMinutes: minutes(durationMs(profile.maxQueueWait)),
    priority: profile.priority,
    warmCount: profile.warmCount,
  };
}

function minutes(ms: number | undefined): number | null {
  return ms === undefined ? null : ms / MINUTE_MS;
}

function hours(ms: number | undefined): number | null {
  return ms === undefined ? null : ms / HOUR_MS;
}

/**
 * The field mask of an update: the paths of the mutable fields that differ
 * from the stored profile. `workload_kind` and `agent_integration_id` never
 * change after creation, so they are never in the mask.
 */
export function profileUpdateMask(
  before: ProfileFormValues,
  after: ProfileFormValues,
): FieldMask {
  const paths: string[] = [];
  if (before.name !== after.name) paths.push("name");
  if (
    after.workloadKind === WorkloadKind.DEV_SESSION &&
    before.workspacePath !== after.workspacePath
  ) {
    paths.push("workspace_path");
  }
  if (before.guestOs !== after.guestOs) paths.push("guest_os");
  if (
    before.guestOs !== after.guestOs ||
    before.sourceKind !== after.sourceKind ||
    before.templateId !== after.templateId
  ) {
    paths.push("source");
  }
  if (
    before.vcpuCount !== after.vcpuCount ||
    before.memoryMib !== after.memoryMib ||
    before.diskGib !== after.diskGib
  ) {
    paths.push("resources");
  }
  if (before.idleTtlMinutes !== after.idleTtlMinutes) paths.push("idle_ttl");
  if (before.maxTtlHours !== after.maxTtlHours) paths.push("max_ttl");
  if (before.priority !== after.priority) paths.push("priority");
  if (before.maxQueueWaitMinutes !== after.maxQueueWaitMinutes) {
    paths.push("max_queue_wait");
  }
  if (before.warmCount !== after.warmCount) paths.push("warm_count");
  return create(FieldMaskSchema, { paths });
}

/** Validates a positive whole number for the per-user dev session limit. */
export function devSessionLimitError(value: number | null): string | undefined {
  if (value === null) return undefined;
  if (!Number.isInteger(value) || value < 1 || value > 2_147_483_647) {
    return "Enter a whole number of at least 1, or clear the field for no limit.";
  }
  return undefined;
}
