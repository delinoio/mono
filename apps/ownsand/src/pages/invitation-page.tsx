// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { useMutation } from "@connectrpc/connect-query";
import { MemberService } from "@delino/ownsand-sdk/gen/organization_pb";
import { Button } from "delino-ui/button";
import { Card, CardActions, CardDescription, CardTitle } from "delino-ui/card";
import { useEffect, useRef, useState } from "react";
import { errorCode, errorMessage } from "../api/errors.ts";
import { AuthStatus, useAuth } from "../auth/auth.tsx";
import { LoadingState, PageHeader } from "../components/common.tsx";
import {
  browserSessionStorage,
  clearPendingSecret,
  keepSecretForSignIn,
  readPendingSecret,
  readSecretFromFragment,
  removeFragment,
} from "../invitation.ts";
import { Link, useRouter } from "../router.tsx";
import { RouteId, organizationPath } from "../routes.ts";

/**
 * Reads the secret from the fragment, or from the sign-in redirect. It has no
 * side effects, because React may call a state initializer twice; the
 * fragment is removed in an effect.
 */
function readInitialSecret(): string | undefined {
  const fromFragment = readSecretFromFragment(window.location.hash);
  if (fromFragment !== undefined) {
    return fromFragment;
  }
  const storage = browserSessionStorage();
  return storage ? readPendingSecret(storage) : undefined;
}

function acceptFailureMessage(error: unknown): string {
  switch (errorCode(error)) {
    case Code.NotFound:
      return "This invitation is not valid. It may have expired, been revoked or already been used. Ask an admin for a new link.";
    case Code.AlreadyExists:
      return "You are already a member of this organization.";
    default:
      return errorMessage(error);
  }
}

export function InvitationPage() {
  const auth = useAuth();
  const { navigate } = useRouter();
  const [secret, setSecret] = useState<string | undefined>(readInitialSecret);
  const [failure, setFailure] = useState<string | undefined>(undefined);
  const accept = useMutation(MemberService.method.acceptInvitation);
  const redirecting = useRef(false);

  // Remove the fragment from the address bar as soon as it is read, whether
  // or not it held a valid secret.
  useEffect(() => {
    if (window.location.hash !== "") {
      removeFragment(window.location, window.history);
    }
  }, []);

  // A signed-out user keeps the secret only for the sign-in redirect.
  useEffect(() => {
    if (
      secret === undefined ||
      auth.status !== AuthStatus.SignedOut ||
      redirecting.current
    ) {
      return;
    }
    const storage = browserSessionStorage();
    if (storage) {
      keepSecretForSignIn(storage, secret);
    }
  }, [secret, auth.status]);

  const signIn = () => {
    redirecting.current = true;
    return auth.signIn();
  };

  const acceptInvitation = async () => {
    if (secret === undefined) {
      return;
    }
    setFailure(undefined);
    try {
      const response = await accept.mutateAsync({ invitationSecret: secret });
      const organizationId = response.organization?.organizationId;
      navigate(
        organizationId
          ? organizationPath(organizationId, RouteId.Sandboxes)
          : "/",
        { replace: true },
      );
    } catch (caught) {
      setFailure(acceptFailureMessage(caught));
    } finally {
      // The secret is deleted when AcceptInvitation returns, also on failure.
      const storage = browserSessionStorage();
      if (storage) {
        clearPendingSecret(storage);
      }
      setSecret(undefined);
    }
  };

  if (auth.status === AuthStatus.Loading) {
    return (
      <main className="page narrow">
        <LoadingState />
      </main>
    );
  }

  return (
    <main className="page narrow">
      <PageHeader title="Accept invitation" />
      {failure ? (
        <Card>
          <CardTitle>The invitation could not be accepted</CardTitle>
          <p className="form-error" role="alert">
            {failure}
          </p>
          <CardActions>
            <Link className="text-link" to="/">
              Go to your organizations
            </Link>
          </CardActions>
        </Card>
      ) : secret === undefined ? (
        <Card>
          <CardTitle>No invitation found</CardTitle>
          <CardDescription>
            Open the invitation link again, or paste it on the organizations
            page.
          </CardDescription>
          <CardActions>
            <Link className="text-link" to="/">
              Go to your organizations
            </Link>
          </CardActions>
        </Card>
      ) : auth.status === AuthStatus.SignedIn ? (
        <Card>
          <CardTitle>Join the organization</CardTitle>
          <CardDescription>
            You will join the organization that sent this invitation with the
            account you are signed in with.
          </CardDescription>
          <CardActions>
            <Button onClick={acceptInvitation} autoFocus>
              Accept invitation
            </Button>
          </CardActions>
        </Card>
      ) : auth.status === AuthStatus.Unconfigured ? (
        <p className="form-error" role="alert">
          Sign-in is not configured for this console.
        </p>
      ) : (
        <Card>
          <CardTitle>Sign in to accept</CardTitle>
          <CardDescription>
            Sign in first. You will come back here to accept the invitation.
          </CardDescription>
          <CardActions>
            <Button onClick={signIn} autoFocus>
              Sign in
            </Button>
          </CardActions>
        </Card>
      )}
    </main>
  );
}
