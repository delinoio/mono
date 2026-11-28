// SPDX-License-Identifier: Apache-2.0
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import ts from "typescript";

const testDirectories = new Set([
  "__fixtures__",
  "__tests__",
  "fixtures",
  "test",
  "tests",
]);

function* implementationSources(
  packageRoot: string,
  directory = "src",
): Generator<string> {
  for (const entry of readdirSync(join(packageRoot, directory), {
    withFileTypes: true,
  })) {
    const path = `${directory}/${entry.name}`;
    if (entry.isDirectory() && !testDirectories.has(entry.name)) {
      yield* implementationSources(packageRoot, path);
    } else if (
      entry.isFile() &&
      /\.(tsx?|css)$/.test(entry.name) &&
      !/\.(test|spec|fixture)\.(tsx?|css)$/.test(entry.name)
    ) {
      yield path;
    }
  }
}

function isUiSdkStylesheet(specifier: string): boolean {
  return /^@ui-sdk\/[^?#]+\.css(?:[?#].*)?$/.test(specifier);
}

const cssComment = String.raw`\/\*[\s\S]*?(?:\*\/|$)`;
const cssString = String.raw`"(?:\\[\s\S]|[^"\\])*"|'(?:\\[\s\S]|[^'\\])*'`;
const cssTrivia = String.raw`(?:\s|${cssComment})*`;
const cssToken = new RegExp(
  String.raw`${cssComment}|${cssString}|\\[\s\S]|@[-\w\u0080-\uffff]+|\s+|.`,
  "gs",
);
const cssImportTarget = new RegExp(
  String.raw`${cssTrivia}(?:(${cssString})|url\(${cssTrivia}(?:(${cssString})|([^\s"'()]+))${cssTrivia}\))`,
  "iy",
);

function cssImportsUiSdkStylesheet(source: string): boolean {
  let depth = 0;
  let atRuleStart = true;
  for (const match of source.matchAll(cssToken)) {
    const token = match[0];
    if (token.startsWith("/*") || /^\s/.test(token)) {
      continue;
    }
    if (atRuleStart && token.toLowerCase() === "@import") {
      cssImportTarget.lastIndex = match.index + token.length;
      const target = cssImportTarget.exec(source);
      if (target) {
        const quoted = target[1] ?? target[2];
        if (isUiSdkStylesheet(quoted ? quoted.slice(1, -1) : target[3])) {
          return true;
        }
      }
    }
    // A CSS import starts a rule at the stylesheet's top level. Track
    // blocks and functions so punctuation inside them cannot start a rule.
    if (token === "{" || token === "(" || token === "[") {
      depth += 1;
    } else if (token === "}" || token === ")" || token === "]") {
      depth -= 1;
    }
    atRuleStart = depth === 0 && (token === ";" || token === "}");
  }
  return false;
}

export function importsUiSdkStylesheet(path: string, source: string): boolean {
  if (path.endsWith(".css")) {
    return cssImportsUiSdkStylesheet(source);
  }
  const file = ts.createSourceFile(path, source, ts.ScriptTarget.Latest);
  return file.statements.some((statement) => {
    if (
      !ts.isImportDeclaration(statement) &&
      !ts.isExportDeclaration(statement)
    ) {
      return false;
    }
    const { moduleSpecifier } = statement;
    return (
      moduleSpecifier !== undefined &&
      ts.isStringLiteral(moduleSpecifier) &&
      isUiSdkStylesheet(moduleSpecifier.text)
    );
  });
}

export function findUiSdkStylesheetImports(packageRoot: string): string[] {
  return Array.from(implementationSources(packageRoot))
    .filter((path) =>
      importsUiSdkStylesheet(
        path,
        readFileSync(join(packageRoot, path), "utf8"),
      ),
    )
    .sort();
}
