// SPDX-License-Identifier: Apache-2.0
import { useHandleSignInCallback } from "@logto/react";
import { useEffect } from "react";
import { ErrorNotice, Loading } from "../components/ui.tsx";
import { RouteId, routePath, useRouter } from "../router.tsx";

/** Route `callback` with Logto: completes the sign-in redirect. */
export function LogtoCallbackPage() {
  const { navigate } = useRouter();
  const { isLoading, error } = useHandleSignInCallback(() => {
    navigate(routePath({ id: RouteId.Organizations }), { replace: true });
  });
  if (error) return <ErrorNotice error={error} />;
  return isLoading ? <Loading label="Signing in" /> : null;
}

/** Route `callback` with the mock API. */
export function MockCallbackPage() {
  const { navigate } = useRouter();
  useEffect(() => {
    navigate(routePath({ id: RouteId.Organizations }), { replace: true });
  }, [navigate]);
  return <Loading label="Signing in" />;
}
