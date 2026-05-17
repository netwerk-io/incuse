# incuse — agent conventions

## Code style
- Go 1.25.x, strict.
- Tabs, double quotes, 80-col soft target (gofmt-driven).
- No `any` without a concrete reason; prefer `unknown`-style typed inputs and
  decode at the boundary.
- Comments explain *why*, not *what*. No comment that restates the next line.

## Layout
- `cmd/incuse` — single binary entrypoint.
- `internal/config` — YAML config + label resolver.
- `internal/incus` — Incus client wrapper (Unix socket primary, HTTPS+cert alt).
- `internal/scaleset` — GitHub Actions Runner Scale Set client + listener glue.
- `internal/runner` — cloud-init template + JIT bootstrap rendering.
- `internal/orchestrator` — wires the above into the JobAssigned → VM lifecycle
  → reap loop.
- `deploy/systemd` — unit file, installer, example config.
- `docs/` — runbooks (deployment, operations, Incus access, runner image).

## Tests
- Source under each package, tests in `_test.go` siblings.
- Test behaviour, not internal state.
- Integration tests against real Incus / GitHub gated behind env vars and
  skipped by default in CI.

## Commits
- Conventional commits: `type(scope): subject` ≤ 72 chars.
- One logical change per commit. Body only when needed.

## Branches
- Work on feature branches. Never commit to `main` directly.
