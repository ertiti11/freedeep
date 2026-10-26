# Contributing to freedeep

Small, focused pull requests are easiest to review.

## Before opening a pull request

```bash
gofmt -w cmd internal
go vet ./...
go test ./...
go test -race ./...
```

Update tests and documentation when behaviour changes. Keep the core dependency-free unless a dependency has a clear security or maintenance benefit that cannot reasonably be achieved with the standard library.

Never commit a real auth export, cookie, token, `.env`, browser profile, account file, private key, or log containing credentials. Use short, obviously fake values in tests.

Install the local checks once after cloning:

```bash
./scripts/install-hooks.sh
```

On Windows PowerShell, run `.\scripts\install-hooks.ps1`. The hooks fail closed when Gitleaks is unavailable.

## Design principles

- preserve protocol compatibility without hiding upstream limitations;
- fail closed when a security setting explicitly requires protection;
- bound memory, concurrency, retries, and request duration;
- keep platform-specific code isolated and tested;
- prefer clear errors over silent fallback behaviour.

By contributing, you agree that your contribution is distributed under the repository's MIT License.
