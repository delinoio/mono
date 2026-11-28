// SPDX-License-Identifier: Apache-2.0
import siteConfig from "../rspress.config";

const outDir = process.env.DELINO_SITE_ARTIFACT_TEST_OUT_DIR;
if (!outDir) {
  throw new Error("DELINO_SITE_ARTIFACT_TEST_OUT_DIR is required");
}

export default { ...siteConfig, outDir };
