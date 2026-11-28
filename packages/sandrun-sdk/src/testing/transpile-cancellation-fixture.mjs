// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { registerHooks } from "node:module";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const sourceRoots = [
  new URL("../", import.meta.url).href,
  new URL("../../../../protos/delino/sandrun/v1/", import.meta.url).href,
];

// The fixture loads TypeScript source, including enums that Node cannot strip.
registerHooks({
  load(url, context, nextLoad) {
    if (
      !url.endsWith(".ts") ||
      !sourceRoots.some((root) => url.startsWith(root))
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
