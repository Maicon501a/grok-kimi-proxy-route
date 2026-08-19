# xAI signup engine provenance

This embedded runtime is an integration-focused adaptation of:

- Repository: https://github.com/xinxinshuhao-create/grok-register
- Commit: `36f379ab2307ca1f718fd6c4502f4c0239317ce0`
- License: MIT

The original multi-script toolkit was reduced to one cancellable, single-account
worker with a structured stdout contract for the Go/Wails host. It retains the
upstream registration flow: dynamic Next.js action discovery, gRPC-Web OTP,
YesCaptcha Turnstile solving, SSO extraction, and SSO-backed OAuth Device Flow
authorization. Runtime credentials are not written to the repository or to
plaintext `keys/*.txt` files.

Configuration is provided through environment variables. See
`internal/register/config.go` and the project README for the supported names.
