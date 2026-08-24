# `@phpcraftdream/rush`

**Unofficial npm distribution of [rush](https://github.com/PHPCraftdream/crush), maintained as a fork of [charmbracelet/crush](https://github.com/charmbracelet/crush). Not published by Charmbracelet.**

This package installs the `rush` CLI as a single prebuilt binary — **no Go
toolchain, no pnpm, no build step** on the user's machine. The correct binary
for your OS/arch is pulled in automatically as an npm optional dependency
(the same distribution model `esbuild` uses).

## Install

```sh
npm install -g @phpcraftdream/rush
```

Then run:

```sh
rush --version
rush
```

## Supported platforms

| npm package                        | OS      | Arch |
| ---------------------------------- | ------- | ---- |
| `@phpcraftdream/rush-linux-x64`   | Linux   | x64  |
| `@phpcraftdream/rush-linux-arm64` | Linux   | arm64 |
| `@phpcraftdream/rush-darwin-x64`  | macOS   | x64  |
| `@phpcraftdream/rush-darwin-arm64`| macOS   | arm64 (Apple Silicon) |
| `@phpcraftdream/rush-win32-x64`   | Windows | x64  |

The launcher (`bin/rush.js`) resolves the matching package and execs its
binary with argv passthrough. If your platform has no package, it exits with
a clear message.

## Scope

Packages publish under the `@phpcraftdream` npm scope.

## Licensing & redistribution

rush is licensed under the **Functional Source License, FSL-1.1-MIT**,
© 2025–2026 Charmbracelet, Inc. The full text is shipped in `LICENSE` and at
the repository root as `LICENSE.md`. FSL permits non-competing use and
redistribution provided the license and copyright notice are preserved; it
converts to MIT after two years. This npm repackaging is an unofficial
convenience distribution and does not change the license of the underlying
software.

## Reporting issues

For bugs in this fork's code or this npm packaging (missing binaries, wrong platform selection, launcher errors), file an issue against this fork at <https://github.com/PHPCraftdream/crush/issues>. For bugs in the code inherited from upstream that also reproduce in [charmbracelet/crush](https://github.com/charmbracelet/crush), report them upstream at <https://github.com/charmbracelet/crush/issues>.
