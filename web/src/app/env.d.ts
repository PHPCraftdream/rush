declare const __GIT_COMMIT__: string;
declare const __GIT_COUNT__: string;
declare const __GIT_BRANCH__: string;

// CSS side-effect imports (main.tsx's ./index.css) are resolved at build
// time by rsbuild; newer TypeScript defaults flag them as unresolved
// modules during `tsc --noEmit`, so declare the wildcard explicitly.
declare module "*.css";
