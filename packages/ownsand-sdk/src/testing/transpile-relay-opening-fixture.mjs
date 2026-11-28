// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { registerHooks } from "node:module";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const sdkSource = new URL("../", import.meta.url).href;
const protoSource = new URL(
  "../../../../protos/delino/ownsand/v1/",
  import.meta.url,
).href;

// Node 26 removed --experimental-transform-types. The fixture needs this
// hook while it imports SDK and binding source with runtime enums.
registerHooks({
  resolve(specifier, context, nextResolve) {
    // Select the binding package's source exports for SDK imports so the
    // fixture runs without a build.
    if (
      context.parentURL?.startsWith(sdkSource) &&
      specifier.startsWith("@delino/ownsand-protos/v1/")
    ) {
      return nextResolve(specifier, {
        ...context,
        conditions: [...context.conditions, "browser"],
      });
    }

    return nextResolve(specifier, context);
  },

  load(url, context, nextLoad) {
    if (
      !url.endsWith(".ts") ||
      !(url.startsWith(sdkSource) || url.startsWith(protoSource))
    ) {
      return nextLoad(url, context);
    }

    const fileName = fileURLToPath(url);
    const { outputText } = ts.transpileModule(readFileSync(fileName, "utf8"), {
      fileName,
      compilerOptions: {
        module: ts.ModuleKind.ESNext,
        target: ts.ScriptTarget.ES2024,
        verbatimModuleSyntax: true,
      },
    });
    return { format: "module", source: outputText, shortCircuit: true };
  },
});
