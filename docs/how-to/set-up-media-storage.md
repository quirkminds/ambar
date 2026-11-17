# Set up media storage

Use this page to create the R2 bucket for an environment. The design is in decision [0006](../decisions/0006-media-storage-on-r2.md) and [Media storage](../explanation/media-storage.md). The API reads the variables at start ([environment](../reference/environment.md)); without them, development runs with uploads off.

| Environment | Bucket name | App origins allowed by CORS |
| --- | --- | --- |
| Development | `<development-bucket-name>` | `http://localhost:5173`, `http://localhost:4321` |
| Production | `<production-bucket-name>` | `https://app.ambar.page`, `https://ambar.page` |

Replace each placeholder with that environment's bucket name from the operator's local deployment notes. Use a non-production bucket while developing. Every bucket is created with the EU jurisdiction, so both environments use the same `{account}.eu.r2.cloudflarestorage.com` endpoint.

Run Wrangler through Bun, pinned: `bunx wrangler@4.151.3`. It is not a dependency of the repository.

## Create the bucket

1. Set `R2_BUCKET` in the local shell to the target environment's bucket name, then sign in: `bunx wrangler@4.151.3 login`.
2. Create the bucket. The jurisdiction can't be changed later.

   ```sh
   bunx wrangler@4.151.3 r2 bucket create "$R2_BUCKET" --jurisdiction eu
   ```

3. Save the CORS rules as `cors.json`, with the origins from the table:

   ```json
   {
     "rules": [
       {
         "allowed": {
           "origins": ["http://localhost:5173", "http://localhost:4321"],
           "methods": ["PUT", "GET"],
           "headers": ["content-type", "x-amz-checksum-sha256"]
         },
         "exposeHeaders": ["ETag"],
         "maxAgeSeconds": 3600
       }
     ]
   }
   ```

   Then apply them:

   ```sh
   bunx wrangler@4.151.3 r2 bucket cors set "$R2_BUCKET" --jurisdiction eu --file cors.json
   ```

   `PUT` is the direct upload, with the file's signed SHA-256 in `x-amz-checksum-sha256`; `GET` lets the app read an original it was redirected to (for example, to copy an image as PNG).
4. Add the lifecycle rules. Unfinished uploads go after a day:

   ```sh
   bunx wrangler@4.151.3 r2 bucket lifecycle add "$R2_BUCKET" incoming-1d incoming/ --expire-days 1 --jurisdiction eu
   bunx wrangler@4.151.3 r2 bucket lifecycle add "$R2_BUCKET" abort-multipart-1d --abort-multipart-days 1 --jurisdiction eu
   ```

5. Check that nothing is public. Both commands must report none:

   ```sh
   bunx wrangler@4.151.3 r2 bucket dev-url get "$R2_BUCKET" --jurisdiction eu
   bunx wrangler@4.151.3 r2 bucket domain list "$R2_BUCKET" --jurisdiction eu
   ```

   Never enable `r2.dev` or connect a custom domain to the bucket: previews are served by the API, originals by presigned URLs.

## Create the API's credentials

Wrangler can't create R2 access keys; use the dashboard.

1. Open **R2 → Manage API tokens → Create Account API token**.
2. Permission **Object Read & Write**, applied to **this bucket only**.
3. The page shows a token value, an access key pair and two S3 endpoints. Copy the access key pair and the **EU** endpoint into the API's environment (`apps/api/.env.local` in development, the service environment on the VPS):

   ```sh
   R2_ENDPOINT=https://<account-id>.eu.r2.cloudflarestorage.com
   R2_ACCESS_KEY_ID=
   R2_SECRET_ACCESS_KEY=
   R2_BUCKET=<bucket-name-for-this-environment>
   ```

   - The endpoint without `.eu` serves buckets without a jurisdiction; an EU bucket isn't found there.
   - The **token value** is for Cloudflare's own API, which the API doesn't use. Don't store it in the environment.
   - The account ID is in `bunx wrangler@4.151.3 whoami` and in the dashboard URL. It isn't a secret.

The secret access key is shown once. Never paste it in chats, issues or logs. To rotate, create a new token, deploy it, then delete the old one.

## Install libvips

Processing runs libvips' command-line tools, with HEIC support:
- **macOS (development):** `brew install vips` (includes libheif).
- **The VPS:** `sudo apt install libvips-tools`.

Check with `vips -l | grep -E "heifload|webpsave"`. Without them, the API in development warns `photo processing off` and uploads stay `uploaded`; in production it refuses to start. The photo tests skip without them.

## Production only

These apply only once `media.ambar.page` and the API run behind Cloudflare:
- `media.ambar.page`: a proxied DNS record to the VPS, a cache rule that caches `/previews/`, and serving stale content turned off.
- A token with **Zone → Cache Purge** for that zone, for erasing moments.
- SSL mode **Full (strict)**.
