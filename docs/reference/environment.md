# Environment variables

Read by `apps/api/main.go` at startup. There are no built-in addresses, URLs or credentials: in development copy `apps/api/.env.example` to `apps/api/.env.local` (ignored by Git, loaded by `main.go`); on the VPS set them in the service environment. Missing required variables stop the API with a message naming them.

| Variable | Required | Purpose |
| --- | --- | --- |
| `ADDR` | yes | Listen address. Behind a reverse proxy keep it on localhost |
| `DATABASE_URL` | yes | PostgreSQL DSN. The `compose.yaml` database is `ambar_dev` |
| `APP_URL` | yes | The app that opens the email link. An `https` URL means production: `AUTH_SECRET`, `RESEND_API_KEY` and `MAIL_FROM` become required and the cookie becomes `__Host-session` |
| `API_URL` | with a provider | Public base URL of the API, exactly as registered at the providers. Redirect URI: `API_URL/v1/auth/oauth/<provider>/callback` |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | no | Google sign-in (web client). Both or neither; neither turns Google off |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | no | GitHub sign-in (OAuth app). Both or neither; neither turns GitHub off |
| `ALLOWED_ORIGINS` | yes | Comma-separated browser origins allowed to call `/v1` and to be used as `returnTo` |
| `AUTH_SECRET` | production | Keys the code and token hashes; 32+ random characters. Development generates one per run when unset |
| `RESEND_API_KEY` | production | Email provider. Unset: sign-in emails are printed to the console |
| `MAIL_FROM` | production | Sender; its domain must be verified in Resend |
| `TRUST_PROXY` | no | `true` only when the API is reachable solely through our reverse proxy, which sets `X-Real-IP` |
| `LOG_FORMAT` | no | `text` (default) or `json` for the log collector on the VPS |
| `R2_ENDPOINT` | production | The bucket's S3 endpoint for its jurisdiction: `https://<account>.eu.r2.cloudflarestorage.com`. With the three below, all or none; none turns uploads off (development only, they answer `storage-unavailable`) |
| `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY` | production | The bucket's R2 access key pair (Object Read & Write on that bucket only). See [Set up media storage](../how-to/set-up-media-storage.md) |
| `R2_BUCKET` | production | The bucket for this environment, a non-production one while developing ([Set up media storage](../how-to/set-up-media-storage.md)) |

Keep this table and `.env.example` in step with `main.go`.
