// Flat ESLint config for the canton-x402 monorepo.
//
// Scope: lint TypeScript sources across all workspace packages. Test
// files use a slightly relaxed ruleset (no-explicit-any allowed for
// mock helpers). Generated dist + Daml build dirs are ignored.

import tseslint from "typescript-eslint";

export default tseslint.config(
  {
    ignores: [
      "**/dist/**",
      "**/node_modules/**",
      "**/.daml/**",
      "**/examples/**", // examples are runnable scripts, not library code
      "packages/daml/**",
      "**/coverage/**",
    ],
  },
  ...tseslint.configs.recommended,
  {
    rules: {
      "@typescript-eslint/no-unused-vars": [
        "warn",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
      "@typescript-eslint/no-explicit-any": "warn",
      "@typescript-eslint/consistent-type-imports": [
        "warn",
        { prefer: "type-imports", fixStyle: "inline-type-imports" },
      ],
      "no-console": ["warn", { allow: ["warn", "error"] }],
      "no-debugger": "error",
    },
  },
  {
    // Tests can be slightly looser — mock typing often needs `any`.
    files: ["**/*.test.ts"],
    rules: {
      "@typescript-eslint/no-explicit-any": "off",
      "@typescript-eslint/no-unused-vars": "off",
    },
  }
);
