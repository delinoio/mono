// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { AccessTokenUnavailableError } from "../auth/auth.tsx";

/**
 * Returns a user-facing message for a failed call. It never includes the raw
 * server message, which can echo request details.
 */
export function errorMessage(error: unknown): string {
  if (error instanceof AccessTokenUnavailableError) {
    return "Your session has ended. Sign in again.";
  }
  const connectError = ConnectError.from(error);
  switch (connectError.code) {
    case Code.Unauthenticated:
      return "Your session has ended. Sign in again.";
    case Code.PermissionDenied:
      return "Your role does not allow this action.";
    case Code.NotFound:
      return "It was not found, or you do not have access to it.";
    case Code.AlreadyExists:
      return "It already exists.";
    case Code.FailedPrecondition:
      return "This action is not possible in the current state.";
    case Code.InvalidArgument:
      return "Some values are not valid. Check them and try again.";
    case Code.ResourceExhausted:
      return "Too many requests. Wait a moment and try again.";
    case Code.Unavailable:
      return "The service is not available. Try again later.";
    default:
      return "Something went wrong. Try again.";
  }
}

/** Returns the Connect code of an error, or `undefined` for other errors. */
export function errorCode(error: unknown): Code | undefined {
  return error instanceof ConnectError ? error.code : undefined;
}
