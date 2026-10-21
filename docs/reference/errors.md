# Errors

Every error response is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem document with `Content-Type: application/problem+json` (decision [0002](../decisions/0002-errors-as-problem-details.md)). The shape is `Problem` in [`openapi.yaml`](../../apps/api/openapi.yaml).

| Member | Always | Meaning |
| --- | --- | --- |
| `type` | yes | `https://docs.ambar.page/errors/<code>` |
| `title` | yes | Short human name of the code, same for every occurrence |
| `status` | yes | HTTP status, repeated from the response |
| `detail` | yes | End-user sentence. Safe to show as-is. Never internal text |
| `code` | yes | Stable identifier, see below. Clients switch on this |
| `requestId` | yes | Same value as the `X-Request-Id` header |
| `attemptsLeft` | `invalid-code` | Wrong codes the user can still try |
| `retryAfterMs` | `rate-limited` | Milliseconds until the next attempt is allowed |

## Codes

| `code` | Status | When |
| --- | --- | --- |
| `invalid-email` | 400 | The email field is missing or malformed |
| `invalid-code` | 400 | The code does not match. Carries `attemptsLeft` |
| `too-many-attempts` | 400 | The code is dead after too many wrong tries; ask for a new one |
| `expired-code` | 400 | The code or link token is expired or already used |
| `bad-request` | 400, 415 | The body is not `application/json` (415) or cannot be decoded (400) |
| `provider-unavailable` | 404 | The sign-in provider is unknown or not configured on this server |
| `unauthorized` | 401 | No valid session cookie or token |
| `forbidden` | 403 | Browser request from an origin that is not allowed |
| `rate-limited` | 429 | Too many emails for this address (carries `retryAfterMs`), too many uploads in progress in a vault (200), or too many requests from this client |
| `network` | 503 | The sign-in email could not be sent. Also sets `Retry-After` |
| `vault-not-found` | 404 | No vault with that id, or the caller isn't a member (the same answer on purpose) |
| `vault-read-only` | 403 | The caller can view the vault but not add to it |
| `upload-not-found` | 404 | No upload with that id for the caller |
| `upload-missing` | 409 | `completeUpload` before the file reached storage |
| `upload-not-pending` | 409 | `cancelUpload` on an upload already past `pending` |
| `upload-expired` | 410 | The upload wasn't completed within an hour; its reservation is released |
| `storage-unavailable` | 503 | Storage can't be reached, or isn't configured (development without `R2_*`). Also sets `Retry-After` |
| `internal` | 500 | Unexpected server error. The log line with the same `requestId` has the cause |

Per-file outcomes of `createUploads` are not problem documents: a rejected file carries `unsupported-type`, `file-too-large` (with `maxBytes`) or `quota-exceeded` (with `bytesLeft`) inside the `201` response, and the other files go on. A failed upload carries `upload-mismatch` in `failure` (`unreadable-file` and `internal` arrive with processing).

## Designed, not served yet

The moments endpoints ([0006](../decisions/0006-media-storage-on-r2.md)) are in the contract but not implemented. Their codes move to the table above in the pull request that serves them.

| `code` | Status | When |
| --- | --- | --- |
| `moment-not-found` | 404 | No moment with that id that the caller can see |
| `invalid-signature` | 403 | A preview URL is expired, altered or signed with a retired key |

## Provider sign-in redirects

Google and GitHub sign-in happen in browser navigations, so their failures are not problem documents: the callback redirects back to the attempt's `returnTo` (default `<APP_URL>/login`) with `error=<code>` added; success adds `oauth=created` or `oauth=signed-in` instead. Clients read both with `readAuthReturn` (`@ambar/core`) and show the copy in `PROVIDER_ERROR_COPY`.

| `error` | When |
| --- | --- |
| `oauth-cancelled` | The person declined at the provider (`access_denied`) |
| `oauth-state` | The attempt is missing, expired (10 minutes) or does not match: an old tab, another browser, or a forged request. Always sent to `<APP_URL>/login`: there is no trusted `returnTo` |
| `oauth-email` | The provider has no verified email for the account (GitHub: unverified primary email) |
| `oauth-taken` | Settings → Connect: that provider account already signs in to another Ambar account |
| `oauth-failed` | Anything else: the provider is down, the code was rejected, or our client configuration is wrong. Logged as `auth.oauth_failed` |

Rules for new codes:

- kebab-case, named after the condition, not the HTTP status.
- Add the row here, the enum value in `openapi.yaml`, the Go case in `authError` (or the relevant handler) and, if clients act on it, the value in `AuthErrorCode` (`@ambar/core`).
- `detail` is copy for end users. Keep it short and actionable.
