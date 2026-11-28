// SPDX-License-Identifier: Apache-2.0
import { EmptySchema } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { expect, test } from "@rstest/core";
import { connectCodeOf, redactError } from "./errors.ts";

const token = "secret-aggregate-token";

function redact(aggregate: AggregateError): AggregateError {
  return redactError(aggregate, [token]) as AggregateError;
}

test("preserves aggregate subclasses, descriptors and diagnostics when cloning", () => {
  class Attempts extends AggregateError {
    override get name() {
      return "Attempts";
    }
  }
  const diagnostic = new Error("IPv4 attempt");
  const aggregate = new Attempts([diagnostic, undefined], `failed: ${token}`, {
    cause: new Error(`connection reset: ${token}`),
  });
  const originalStack = `${aggregate.stack}\nfixture stack frame`;
  aggregate.stack = originalStack;

  const copy = redact(aggregate);

  expect(copy).toBeInstanceOf(Attempts);
  expect(copy.name).toBe("Attempts");
  expect(copy.message).toBe("failed: [redacted]");
  expect(copy.stack).toBe(originalStack.replaceAll(token, "[redacted]"));
  expect((copy.cause as Error).message).toBe("connection reset: [redacted]");
  expect(Object.getOwnPropertyDescriptor(copy, "errors")).toEqual({
    value: [diagnostic, undefined],
    enumerable: false,
    configurable: true,
    writable: true,
  });
  expect(copy.errors[0]).toBe(diagnostic);
  expect(aggregate.message).toContain(token);
});

test("redacts a changed cause and preserves clean aggregate entries", () => {
  const aggregate = new AggregateError(["network diagnostic"], "failed", {
    cause: `connection reset: ${token}`,
  });

  const copy = redact(aggregate);

  expect(copy).toBeInstanceOf(AggregateError);
  expect(copy.errors).toEqual(["network diagnostic"]);
  expect(copy.cause).toBe("connection reset: [redacted]");
});

test("keeps a redacted stack when the original stack cannot be read", () => {
  const aggregate = new AggregateError([], `failed: ${token}`);
  Object.defineProperty(aggregate, "stack", {
    get() {
      throw new Error(token);
    },
  });

  expect(redact(aggregate).stack).toContain("failed: [redacted]");
});

test("redacts nested aggregates and Connect errors while retaining status and details", () => {
  const details = [{ desc: EmptySchema, value: {} }];
  const connect = new ConnectError(
    `remote: ${token}`,
    Code.Unavailable,
    { "x-test": "example" },
    details,
    `connection reset: ${token}`,
  );
  const nested = new AggregateError([connect, `fallback: ${token}`], "nested");
  const copy = redact(new AggregateError([nested], "outer"));

  const inner = copy.errors[0] as AggregateError;
  const checked = inner.errors[0] as ConnectError;
  expect(inner).toBeInstanceOf(AggregateError);
  expect(inner.errors[1]).toBe("fallback: [redacted]");
  expect(checked).toBeInstanceOf(ConnectError);
  expect(checked.rawMessage).toBe("remote: [redacted]");
  expect(checked.cause).toBe("connection reset: [redacted]");
  expect(connectCodeOf(checked)).toBe(Code.Unavailable);
  expect(connectCodeOf(copy)).toBeUndefined();
  expect(checked.metadata.get("x-test")).toBe("example");
  expect(checked.details).toBe(details);
});

test("keeps clean graphs with shared entries by reference", () => {
  const shared = new Error("diagnostic");
  const aggregate = new AggregateError(
    [shared, undefined, shared, NaN],
    "clean",
    {
      cause: shared,
    },
  );

  expect(redact(aggregate)).toBe(aggregate);
});

test("drops cycles through causes or aggregate entries", () => {
  const aggregate = new AggregateError([], "outer");
  const circular = new Error(`back edge: ${token}`, { cause: aggregate });
  Object.defineProperty(aggregate, "cause", { value: aggregate });
  aggregate.errors.push(aggregate, undefined, circular);

  const copy = redact(aggregate);

  expect(copy.cause).toBeUndefined();
  expect(copy.errors).toHaveLength(2);
  expect(copy.errors[0]).toBeUndefined();
  expect(copy.errors[1].message).toBe("back edge: [redacted]");
  expect(copy.errors[1].cause).toBeUndefined();
});

test("drops causes and aggregate entries beyond the depth limit", () => {
  let aggregate = new AggregateError([`entry: ${token}`], "leaf", {
    cause: `cause: ${token}`,
  });
  for (let depth = 0; depth < 8; depth++) {
    aggregate = new AggregateError([aggregate], "branch");
  }

  let copy = redact(aggregate);
  for (let depth = 0; depth < 8; depth++) {
    copy = copy.errors[0];
  }
  expect(copy.message).toBe("leaf");
  expect(copy.errors).toEqual([]);
  expect(copy.cause).toBeUndefined();
});

test("bounds visits to wide arrays and shared subgraphs", () => {
  const tail = new Error(token);
  const wide = new AggregateError([...Array(1000).fill(null), tail], "wide");
  let reads = 0;
  Object.defineProperty(wide.errors, 1000, {
    get() {
      reads++;
      return tail;
    },
  });
  const copy = redact(wide);
  expect(reads).toBe(0);
  expect(copy.errors).toEqual(Array(63).fill(null));

  let branch: Error = new Error("clean leaf");
  for (let depth = 0; depth < 4; depth++) {
    branch = new AggregateError(Array(20).fill(branch), "branch");
  }
  const count = (error: Error): number =>
    error instanceof AggregateError
      ? 1 + error.errors.reduce((total, child) => total + count(child), 0)
      : 1;
  expect(count(redact(branch as AggregateError))).toBe(64);
});

test("replaces a non-array or throwing errors property with an empty array", () => {
  for (const descriptor of [
    { value: token },
    {
      get() {
        throw new Error(token);
      },
    },
  ]) {
    const aggregate = new AggregateError([], "fetch failed");
    Object.defineProperty(aggregate, "errors", descriptor);

    const copy = redact(aggregate);

    expect(copy).toBeInstanceOf(AggregateError);
    expect(copy.errors).toEqual([]);
    expect(Object.getOwnPropertyDescriptor(copy, "errors")?.enumerable).toBe(
      false,
    );
  }
});

test("keeps readable entries when one aggregate slot throws", () => {
  const aggregate = new AggregateError(["unreadable", `readable: ${token}`]);
  Object.defineProperty(aggregate.errors, 0, {
    get() {
      throw new Error(token);
    },
  });

  expect(redact(aggregate).errors).toEqual(["readable: [redacted]"]);
});
