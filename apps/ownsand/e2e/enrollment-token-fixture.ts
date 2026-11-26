// SPDX-License-Identifier: Apache-2.0
import { type Timestamp } from "@bufbuild/protobuf/wkt";
import { type Code, ConnectError, createClient } from "@connectrpc/connect";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { createMockTransport, MOCK_IDS } from "../src/mock/mock-api.ts";

const client = createClient(DeviceService, createMockTransport());
const organizationId = MOCK_IDS.acme;
const timestampNanos = ({ seconds, nanos }: Timestamp) =>
  (seconds * 1_000_000_000n + BigInt(nanos)).toString();

async function createEnrollmentToken(
  timeMs: number,
  ttl: { seconds: number; nanos: number } | null,
): Promise<{ id: string; created: string; expires: string } | { code: Code }> {
  const realNow = Date.now;
  Date.now = () => timeMs;
  try {
    const { token } = await client.createEnrollmentToken({
      organizationId,
      ttl: ttl ? { seconds: BigInt(ttl.seconds), nanos: ttl.nanos } : undefined,
    });
    if (!token?.createTime || !token.expireTime) {
      throw new Error("CreateEnrollmentToken returned no timestamps");
    }
    return {
      id: token.enrollmentTokenId,
      created: timestampNanos(token.createTime),
      expires: timestampNanos(token.expireTime),
    };
  } catch (error) {
    if (error instanceof ConnectError) return { code: error.code };
    throw error;
  } finally {
    Date.now = realNow;
  }
}

async function listEnrollmentTokenIds() {
  const { tokens } = await client.listEnrollmentTokens({ organizationId });
  return tokens.map((token) => token.enrollmentTokenId);
}

declare global {
  interface Window {
    enrollmentTokenFixture: {
      create: typeof createEnrollmentToken;
      listIds: typeof listEnrollmentTokenIds;
    };
  }
}

window.enrollmentTokenFixture = {
  create: createEnrollmentToken,
  listIds: listEnrollmentTokenIds,
};

const ready = document.createElement("p");
ready.textContent = "Enrollment token RPC fixture ready";
document.body.append(ready);
