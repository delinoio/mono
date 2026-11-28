// SPDX-License-Identifier: Apache-2.0
import { CredentialService } from "@delino/sandcrew-protos/v1/credential_pb";
import { NotificationService } from "@delino/sandcrew-protos/v1/notification_pb";
import { OrganizationService } from "@delino/sandcrew-protos/v1/organization_pb";
import { useAllPages } from "./api.ts";

/** The SandRun DEV_SESSION runner profiles with their SandCrew eligibility. */
export function useRunnerProfiles(organizationId: string) {
  return useAllPages(
    OrganizationService.method.listRunnerProfiles,
    { organizationId },
    (r) => ({ items: r.profiles, nextPageToken: r.nextPageToken }),
  );
}

/** The organization credentials and the caller's own, without values. */
export function useVendorCredentials(organizationId: string) {
  return useAllPages(
    CredentialService.method.listVendorCredentials,
    { organizationId },
    (r) => ({ items: r.credentials, nextPageToken: r.nextPageToken }),
  );
}

/** The caller's session secrets, without values. */
export function useSessionSecrets(organizationId: string) {
  return useAllPages(
    CredentialService.method.listSessionSecrets,
    { organizationId },
    (r) => ({ items: r.secrets, nextPageToken: r.nextPageToken }),
  );
}

/** The caller's Web Push subscriptions, without their keys. */
export function usePushSubscriptions() {
  return useAllPages(
    NotificationService.method.listPushSubscriptions,
    {},
    (r) => ({ items: r.subscriptions, nextPageToken: r.nextPageToken }),
  );
}
