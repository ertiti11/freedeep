# Security policy

## Reporting a vulnerability

Please report security issues privately through GitHub's **Security** tab by opening a private vulnerability report. Do not open a public issue for a vulnerability that could expose credentials, bypass proxy authentication, or affect other users.

Include the affected version, impact, minimal reproduction, and any suggested mitigation. Remove all real tokens, cookies, account identifiers, IP addresses, and local paths from the report.

## Credential incidents

DeepSeek browser credentials grant access to the associated Web session. If a token, cookie export, `.env`, browser profile, or `deepseek-auth.json` is ever exposed:

1. revoke or invalidate the session immediately;
2. rotate any independent `PROXY_API_KEY`;
3. remove the material from Git history, release assets, logs, and caches;
4. verify the cleaned history with a secret scanner before pushing again.

Deleting a secret in a later commit does not make the original value safe.

## Supported versions

Security fixes are applied to the latest release. This project depends on an unstable private Web API, so older builds are not maintained as long-term support branches.
