# Contributing to Spawnly

Thanks for your interest! Spawnly is a personal project, so contributions are
reviewed as time allows — issues and small, focused PRs are the easiest to land.

## Getting set up

The quickest path is the [Claude Code plugin](PLUGIN.md) (`/spawnly:up`), or
the manual steps in the [README](README.md). You need Docker, `kind`,
`kubectl`, Go (see `go.work`), and Node.js.

## Making changes

1. Open an issue first for anything larger than a bug fix, so we can agree on
   the approach.
2. Branch from `main`, keep the PR scoped to one change, and use
   [Conventional Commits](https://www.conventionalcommits.org/) style messages
   (`feat(registry): …`, `fix(sidecar): …`) — matching the existing history.
3. Run the fast checks before pushing: `make test` (Go unit tests), plus
   `npm test` in any TypeScript package you touched.
4. The full e2e lane (kind + SPIRE + SpiceDB) runs on `main`; add the
   `full-e2e` label to a PR to run it there.

## Licensing

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), the same as the rest of the project.

## Security issues

Please don't file these as public issues — see [SECURITY.md](SECURITY.md).
