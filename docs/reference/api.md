# API reference

The contract is [`apps/api/openapi.yaml`](../../apps/api/openapi.yaml) (OpenAPI 3.1). It is the source of truth for request and response shapes. The Go tests validate responses they exercise against the contract; an operation being present in OpenAPI does not by itself mean the server implements it. Client schemas are defined in `@ambar/core`.

Base URL: `https://api.ambar.page`. All endpoints under `/v1` are versioned by path; `/healthz` is not.

## Implemented routes

| Method and path | Operation | Purpose |
| --- | --- | --- |
| `GET /healthz` | `getHealth` | Check API availability and ping PostgreSQL |
| `POST /v1/auth/email` | `requestSignInCode` | Email a sign-in code and link |
| `POST /v1/auth/email/verify` | `verifySignIn` | Exchange a code or link token for a session |
| `GET /v1/auth/session` | `getSession` | Who is signed in |
| `DELETE /v1/auth/session` | `signOut` | End the session |
| `GET /v1/auth/providers` | `listSignInProviders` | List configured OAuth sign-in providers |
| `GET /v1/auth/oauth/{provider}` | `startProviderSignIn` | Start a browser redirect to Google or GitHub |
| `GET /v1/auth/oauth/{provider}/callback` | `finishProviderSignIn` | Handle the provider redirect and return to the app |
| `GET /v1/auth/identities` | `listIdentities` | List sign-in identities connected to the current account |
| `DELETE /v1/auth/identities/{provider}` | `disconnectIdentity` | Disconnect a provider identity |
| `POST /v1/uploads` | `createUploads` | Check, reserve and sign one direct-to-R2 `PUT` per file |
| `GET /v1/uploads/{uploadId}` | `getUpload` | Follow an upload |
| `DELETE /v1/uploads/{uploadId}` | `cancelUpload` | Give up a pending upload and release its bytes |
| `POST /v1/uploads/{uploadId}:complete` | `completeUpload` | Say the `PUT` finished; the API checks what arrived |

The OAuth start and callback routes are browser navigations, not JSON fetch endpoints. Email sign-in and OAuth use the same account and session model. Private identity and upload routes require the current session. Uploads stop at `uploaded` for now: processing a file into a moment is not served yet.

## Designed, not served yet

The moment operations below are included in OpenAPI as a proposed contract. The Go router does not implement them yet; clients continue to use the mock upload and library. See [decision 0006](../decisions/0006-media-storage-on-r2.md) and [Media storage](../explanation/media-storage.md).

| Method and path | Operation | Purpose |
| --- | --- | --- |
| `GET /v1/moments` | `listMoments` | A vault's moments (or its Trash), newest first, with signed preview URLs |
| `GET /v1/moments/{momentId}` | `getMoment` | One moment with fresh preview URLs |
| `GET /v1/moments/{momentId}/original` | `getMomentOriginal` | Redirect to the original in R2 (presigned, 15 minutes) |
| `GET media.ambar.page/previews/{vaultId}/{momentId}/{variant}` | `getPreview` | A signed preview image, cached by Cloudflare |

Conventions:

- Requests and responses are JSON. Errors are problem documents; see [errors.md](errors.md).
- Every response carries `X-Request-Id`; see [logs.md](logs.md).
- `/v1` responses are `Cache-Control: no-store`. Browser state-changing requests must use an allowed `Origin`; browser reads also need an allowed origin for CORS.

Lint the file with `bun run lint:openapi`. Validate it against the OpenAPI schema and against the handlers with `bun run test:api`.
