# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
discussions, or pull requests.**

Instead, report them privately by email to:

**omnicore@flyntech.com.br**

If you prefer, you may also use GitHub's private
[Security Advisories](https://github.com/ClaudioSchirmer/omnicore/security/advisories/new)
("Report a vulnerability").

Please include as much of the following as you can, so we can triage quickly:

- a description of the vulnerability and its impact;
- the affected version (or commit) and the build tags in use
  (relational engine + transport);
- steps to reproduce, or a proof-of-concept;
- any known mitigations or workarounds.

## What to expect

- **Acknowledgement** within **3 business days** of your report.
- An initial **assessment** (severity + whether we can reproduce it) within
  **10 business days**.
- We will keep you informed of progress toward a fix and coordinate a
  disclosure timeline with you.
- With your permission, we are happy to credit you once a fix is released.

Please give us a reasonable opportunity to address the issue before any public
disclosure (coordinated disclosure).

## Supported versions

omnicore is pre-1.0 (`0.x`). Security fixes are applied to the **latest released
minor version** only; there is no back-porting to earlier `0.x` releases. Always
upgrade to the most recent release to receive security fixes.

## Scope

This policy covers the omnicore framework module itself
(`github.com/ClaudioSchirmer/omnicore`). Vulnerabilities in third-party
dependencies should be reported to the respective projects, though we welcome a
heads-up so we can bump the dependency.
