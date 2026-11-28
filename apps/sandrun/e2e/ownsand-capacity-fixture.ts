// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import { OwnSandLinkState } from "@delino/sandrun-protos/v1/common_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import { WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const OWN_SAND_CAPACITY_FIXTURE = "sandrun-e2e-ownsand-capacity";
export const REPLACEMENT_ORG = "0193a000-0000-7000-8000-00000000aaaa";
export const REPLACEMENT_DEVICE = "replacement-build-box";

type ReplacementCapacity = "device" | "error" | "empty";

function holdCapacity() {
  let release!: (result: ReplacementCapacity) => void;
  const promise = new Promise<ReplacementCapacity>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

export function ownSandCapacityFixture(base: Transport) {
  let ownsandOrganizationId: string | undefined = MOCK_IDS.ownsandAcme;
  let rejectRemoval = false;
  let replacementCapacity = holdCapacity();
  const capacityReads = { original: 0, replacement: 0, other: 0 };
  const fixture = createRouterTransport((router) => {
    router.service(OwnSandLinkService, {
      getOwnSandLink(request) {
        if (request.organizationId === MOCK_IDS.acme) {
          return ownsandOrganizationId === undefined
            ? {}
            : {
                link: {
                  organizationId: request.organizationId,
                  ownsandOrganizationId,
                  state: OwnSandLinkState.ACTIVE,
                  credentialSet: true,
                },
              };
        }
        return {
          link: {
            organizationId: request.organizationId,
            ownsandOrganizationId: MOCK_IDS.ownsandNorthwind,
            state: OwnSandLinkState.ACTIVE,
            credentialSet: true,
          },
        };
      },
      setOwnSandLink(request) {
        ownsandOrganizationId = request.ownsandOrganizationId;
        return {};
      },
      removeOwnSandLink() {
        if (rejectRemoval) {
          rejectRemoval = false;
          throw new ConnectError(
            "Removal rejected by fixture",
            Code.PermissionDenied,
          );
        }
        ownsandOrganizationId = undefined;
        return {};
      },
    });
    router.service(WorkService, {
      async getCapacity(request) {
        if (request.organizationId === MOCK_IDS.northwind) {
          capacityReads.other++;
          return { devices: [{ name: "northwind-build-box" }] };
        }
        if (ownsandOrganizationId === undefined) {
          throw new ConnectError(
            "OwnSand link required",
            Code.FailedPrecondition,
          );
        }
        capacityReads.replacement++;
        const result = await replacementCapacity.promise;
        if (result === "error") {
          throw new ConnectError("Capacity unavailable", Code.Unknown);
        }
        return result === "empty"
          ? {}
          : {
              devices: [
                {
                  ownsandDeviceId: "0193a000-0000-7000-8000-00000000aaad",
                  name: REPLACEMENT_DEVICE,
                },
              ],
            };
      },
    });
  });

  const transport: Transport = {
    ...base,
    unary(method, ...args) {
      if (method.parent === OwnSandLinkService) {
        return fixture.unary(method, ...args);
      }
      if (
        method.parent === WorkService &&
        method.name === WorkService.method.getCapacity.name
      ) {
        if (
          ("organizationId" in args[3] &&
            args[3].organizationId === MOCK_IDS.northwind) ||
          ownsandOrganizationId !== MOCK_IDS.ownsandAcme
        ) {
          return fixture.unary(method, ...args);
        }
        capacityReads.original++;
      }
      return base.unary(method, ...args);
    },
  };
  return {
    transport,
    capacityReads,
    rejectNextRemoval() {
      rejectRemoval = true;
    },
    releaseCapacity(result: ReplacementCapacity) {
      replacementCapacity.release(result);
      replacementCapacity = holdCapacity();
    },
  };
}
