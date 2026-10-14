import assert from "node:assert/strict";
import ts from "typescript";
import { fileURLToPath } from "node:url";

// Read the public declarations themselves: a stale SDK type must fail even
// when fetch faithfully returns a new editor field at runtime.
const path = fileURLToPath(new URL("index.ts", import.meta.url));
const program = ts.createProgram([path], { target: ts.ScriptTarget.ES2022, strict: true });
export const checker = program.getTypeChecker();
const source = program.getSourceFile(path)!;
export function publicType(name: string): ts.Type {
  const declaration = source.statements.find((s) =>
    (ts.isInterfaceDeclaration(s) || ts.isTypeAliasDeclaration(s)) && s.name.text === name);
  assert.ok(declaration, `missing public type ${name}`);
  return checker.getTypeAtLocation(declaration);
}

export function assertShape(value: unknown, name: string) {
  function errors(value: any, type: ts.Type, path: string): string[] {
    if (type.flags & (ts.TypeFlags.Any | ts.TypeFlags.Unknown)) return [];
    if (type.isUnion()) {
      const options = type.types.map((t) => errors(value, t, path));
      return options.some((e) => e.length === 0) ? [] : [
        `${path}: does not match ${checker.typeToString(type)}`,
        ...options.reduce((shortest, next) => next.length < shortest.length ? next : shortest),
      ];
    }
    if (type.flags & ts.TypeFlags.Null) return value === null ? [] : [`${path}: expected null`];
    if (type.flags & ts.TypeFlags.Undefined) return value === undefined ? [] : [`${path}: expected undefined`];
    const primitive = type.flags & ts.TypeFlags.StringLike ? "string"
      : type.flags & ts.TypeFlags.NumberLike ? "number"
      : type.flags & ts.TypeFlags.BooleanLike ? "boolean" : undefined;
    if (primitive) {
      if (typeof value !== primitive) return [`${path}: expected ${primitive}`];
      if (type.isLiteral() && value !== type.value) return [`${path}: expected ${type.value}`];
      if (type.flags & ts.TypeFlags.BooleanLiteral && String(value) !== checker.typeToString(type)) return [`${path}: wrong boolean`];
      return [];
    }
    if (checker.isArrayType(type)) {
      if (!Array.isArray(value)) return [`${path}: expected array`];
      const [element] = checker.getTypeArguments(type as ts.TypeReference);
      return value.flatMap((v, i) => errors(v, element, `${path}[${i}]`));
    }
    if (!value || typeof value !== "object" || Array.isArray(value)) return [`${path}: expected object`];
    const props = checker.getPropertiesOfType(type);
    const index = checker.getIndexTypeOfType(type, ts.IndexKind.String);
    const result = props.flatMap((prop) => {
      if (!(prop.name in value) && prop.flags & ts.SymbolFlags.Optional) return [];
      return errors(value[prop.name], checker.getTypeOfSymbolAtLocation(prop, prop.valueDeclaration!), `${path}.${prop.name}`);
    });
    for (const key of Object.keys(value)) {
      if (props.some((p) => p.name === key)) continue;
      result.push(...(index ? errors(value[key], index, `${path}.${key}`) : [`${path}.${key}: missing from SDK type`]));
    }
    return result;
  }
  assert.deepEqual(errors(value, publicType(name), name), []);
}
