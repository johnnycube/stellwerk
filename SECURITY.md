# Security policy

## Supported versions

Security fixes go into the latest release. stellwerk is pre-1.0, so please upgrade to the newest version before reporting.

## Reporting a vulnerability

Please do not open a public issue. Report it privately through GitHub's [private vulnerability reporting](https://github.com/johnnycube/stellwerk/security/advisories/new) instead.

Include what you found, how to reproduce it and which version you used. You will get an answer within a week. Once a fix is released, the advisory is published with credit to you, unless you prefer otherwise.

## Scope

Of particular interest:

- Source authentication: tokens, HMAC signatures, rate limits
- Matrix end-to-end encryption, key storage and cross-signing
- Secrets showing up in logs, the audit log, the UI or `-print-config`
- The UI and API listeners
- Release integrity: image signatures, SBOMs, checksums
