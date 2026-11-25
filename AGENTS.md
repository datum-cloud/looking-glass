# Repository guidelines

The Datum Cloud Looking Glass is a full-stack repository: Go services and
Kubernetes controllers, a TypeScript web frontend, and Kubernetes manifests to
deploy them. See [README.md](README.md) for what it provides.

## Commands

The [Development](README.md#development) section of the README lists the
tasks and the local kind environment. Run `task ci` before opening a pull
request; it runs the same verify, lint, build and test steps as CI.

## Code standards

- Go: standard formatting and tab indentation; keep imports grouped by
  standard library, third-party and local packages.
- TypeScript: two-space indentation, single quotes, semicolons and a 100-column
  target.
- Kubernetes and other YAML: two-space indentation. Use `.yaml`, never `.yml`.
- Regenerate generated code, CRDs and manifests through the project's tasks;
  never hand-edit generated files.
- Name Go tests `*_test.go` with `TestXxx` functions. Add regression coverage
  for every behavior change.

## Security

- Keep credentials, kubeconfigs, keys and `.env` files out of commits.
- Use synthetic data in fixtures, docs and tests; never real hostnames, IP
  addresses or tenant identifiers.

## Commits and pull requests

- Use Conventional Commits (`feat:`, `fix:`, optional scope such as
  `fix(api):`). Keep the subject at most 72 characters.
- Use descriptive branch names such as `fix/issue-42`.
- In the pull request description, state the problem, the behavior change,
  the linked issues and the validation you ran. Include screenshots for UI
  changes.
