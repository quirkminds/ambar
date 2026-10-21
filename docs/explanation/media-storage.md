# Media storage

How photos, clips and PDFs are uploaded, processed, stored and shown, and why it is shaped this way. The decision is [0006](../decisions/0006-media-storage-on-r2.md); the endpoints are in [`openapi.yaml`](../../apps/api/openapi.yaml) under the `uploads` tag.

Status: partly built. Vaults, membership and uploads up to `uploaded` (reserve, sign, `:complete`, cancel, cleanup) run in the API; processing, moments and serving are designed, not built, and the clients still use the mock upload and library. The [architecture diagram](../architecture/media-platform.svg) is a future target, not a deployment inventory.

## The split

The API decides; Cloudflare moves the bytes.

| Where | What | Why there |
| --- | --- | --- |
| Client | Reads capture date and dimensions, strips the rest of the metadata, computes the fingerprint, uploads at most 4 files at a time | The location never leaves the device; the bytes never touch the VPS |
| API (Go, VPS) | Session, vault permission, quota, limits, signing, processing, Postgres | Needs the database, makes decisions, keeps state |
| R2 (EU jurisdiction planned) | Every file, private | Avoids serving object bytes from the VPS; verify the bucket jurisdiction and current provider terms before making a residency claim |
| Cloudflare CDN (`media.ambar.page`) | Caches previews the API signed and served | No charge per request; a cached preview never reaches the VPS |

Rule of thumb for future pieces: it goes to Cloudflare when it has to happen near the user (cache, signature checks), moves a lot of bytes, is the security perimeter (TLS, firewall), or costs next to nothing per use. Everything that needs the database, decides or keeps state stays in Go.

## Uploading

1. The client asks for upload slots: `POST /v1/uploads` with up to 20 files. For each one it sends:
   - name, size and type;
   - its SHA-256 fingerprint;
   - what it read before stripping (capture date, width, height, duration).
2. The API checks:
   - the session, and the role in the vault (`owner` or `editor`);
   - each file's type and size;
   - the quota.

   Then, for each file, it does one of three things:
   - **Accepted:** it reserves the bytes and returns a presigned `PUT` (15 minutes, one key under `incoming/`) signed for the declared size, type and SHA-256, so storage takes no other bytes and the URL can't replace the file once it is up.
   - **Duplicate:** the same fingerprint already exists in this vault, so it answers with that moment and nothing is uploaded.
   - **Rejected:** it gives the reason.

   One bad file never blocks the others.
3. The client `PUT`s each accepted file to R2 with the settable signed headers. The browser supplies `Content-Length`; client code must not try to set it manually.
4. The client calls `POST /v1/uploads/{uploadId}:complete`. The API checks that the object exists with the declared size and queues the upload for processing. Calling it again is harmless.
5. The client follows `GET /v1/uploads/{uploadId}` until it reads `ready` (with the moment) or `failed` (with a code).

The reservation is what makes quota hold when 500 files arrive at once: every slot counts against the quota the moment it is signed, not when it finishes.

### What signing pins down

The proposed presigned `PUT` signs the content type and, if supported by R2, the expected size. Declared values are still only declarations. Processing:
- checks the real type from the first bytes;
- recomputes the fingerprint;
- rejects anything that doesn't match (`upload-mismatch`).

### Uploads that never finish

- If the tab closes before `:complete`, the API receives no completion notice. The client has an hour to call `:complete` (the signed `PUT` itself works 15 minutes). After that, the hourly cleanup in `main.go` marks the upload `expired`, which releases its reservation, and deletes its object if one arrived. It removes `expired` and `failed` rows a day later.
- An R2 lifecycle rule should delete anything left in `incoming/` after one day. This rule is a deployment requirement, not current configuration.

### Size

Files are capped at 100 MB and clips at 30 seconds (the client already enforces both). That fits a single `PUT`. Each slot carries `method: "put"`, so a later `multipart` method (resumable, one URL per part) can be added for larger files without breaking clients.

## Processing

There is no queue yet. After `:complete`, the upload is `uploaded`, and workers inside the API process take it with `SELECT … FOR UPDATE SKIP LOCKED`. A restart picks up whatever was left. Processing is idempotent and keyed by upload id, so running it twice changes nothing.

For each upload, processing does this:
1. Reads `incoming/{uploadId}`, checks the type from its bytes and recomputes the SHA-256.
2. Strips metadata **without re-encoding** the original:
   - **JPEG:** drop the APP segments.
   - **PNG and WebP:** drop the text and EXIF chunks.
   - **Clips:** `ffmpeg -map_metadata -1 -c copy`, a remux with no re-encoding.
   - **PDFs:** clear the document info (author, producer).
   - Orientation is kept, so photos are never shown sideways.
3. Writes the clean original to `vaults/{vaultId}/moments/{momentId}/original`.
4. Makes the derived sizes with libvips, which re-encodes. That is fine for copies made only to be shown.
   - Thumbnails come in 480 and 1200 px.
   - HEIC also gets a display copy, because most browsers can't show HEIC.
   - A clip gets a poster.
   - A PDF gets its first page.
5. Deletes `incoming/{uploadId}`, creates the moment and marks the upload `ready`.

Readings (OCR) and AI descriptions are later, separate steps.

Re-encoding the original with libvips would cost quality on every save, which is wrong for a product that keeps the moment as it was.

## Fingerprints

The fingerprint is the SHA-256 of the bytes the client uploads, after the client stripped them. It serves three purposes:
- **Skip duplicate uploads:** checked in `POST /v1/uploads`.
- **Catch lying or broken clients:** recomputed in processing.
- **Integrity:** the stored fingerprint describes the file that was uploaded.

The clean original that processing stores can differ from it by the metadata the API removed again; its own hash is stored separately when needed.

- **Only within one vault, never across.** A global check would tell anyone whether someone else has a given file, just by seeing their upload come back as "already there".
- **Large files:** `crypto.subtle.digest` needs the whole file in memory. At 100 MB that's acceptable; above it, use a streaming hash (`hash-wasm`) or let the server dedupe.
- **Near duplicates** (the same capture re-compressed) are out of scope; a perceptual hash can be added later.

## Personal data

- **Metadata:** the client keeps only capture date and dimensions, and strips the rest before uploading. On JPEG it works at the byte level, never by redrawing on a canvas, which re-compresses. HEIC can't be stripped cheaply in a browser, so for HEIC the server's pass is the only one. The server always strips again, because a client can skip it.
- **Names:** R2 keys are ids. The original file name, which can contain a person's name, lives in Postgres.
- **Logs:** never upload URLs, media URLs, signatures, fingerprints or file names. Ids are enough to trace a request.
- **Derived data:** readings and AI descriptions are personal data too. They live in Postgres and go when the moment goes.
- **At rest:** R2 encrypts stored objects. This does not by itself establish the location of processing or edge delivery.

## AI provider requirements

Ambar will only select AI providers that meet its zero-data-retention (ZDR) requirement. Privacy protection and applicable compliance obligations are design requirements, not a claim that a provider has been selected or that the target has been audited.

Before sending user content to a provider, verify and document its ZDR terms, processing region, training use, subprocessors, gateway logging and deletion behavior. The diagram's EU-processing and no-content-logging statements are not guarantees for the current system. The current system does not send media or readings to AI providers.

## Serving

The bucket has no public access: no `r2.dev`, no custom domain on the bucket. Every read starts with the API deciding that the caller may see the moment (session or agent key, then vault membership or a share) and signing URLs for it. A signed URL is proof of that decision, valid for a bounded time.

The first version uses no Worker. Two kinds of URL, chosen by weight:

| What | URL | Checked by | Valid for | Cost per view |
| --- | --- | --- | --- | --- |
| Previews (480 and 1200 px; photos, clip posters, PDF first pages, link captures) | `https://media.ambar.page/previews/{vaultId}/{momentId}/{variant}?exp&kid&sig` | The API, then cached by Cloudflare's CDN | 24 hours | None on a cache hit; one R2 read and VPS bandwidth on a miss |
| Originals (photos, clips, PDFs) | `GET /v1/moments/{momentId}/original` redirects to an R2 presigned `GET` | R2 | 15 minutes | One R2 read; no egress fee, no VPS bandwidth |

Clips never go through the CDN: Cloudflare's self-serve CDN terms don't allow serving video through it without a paid video product. Previews are small images.

### Preview URLs

- `sig` is HMAC-SHA256 with `MEDIA_SIGNING_KEY` over the path and `exp`, encoded as base64url. The API signs and checks it with `crypto/hmac` (`hmac.Equal`, constant time). The key exists only in `apps/api`.
- The key is its own secret, not derived from `AUTH_SECRET`, so each one rotates on its own. `kid` names the key, so the previous one keeps verifying during a rotation.
- `exp` is rounded up to the next 24-hour boundary, at least 12 hours away. A URL is valid for 12 to 36 hours and stays identical within its window, so browsers and the CDN reuse it.
- Fix the signed string and keep shared test vectors in the repository, so a later verifier (a Worker) can be checked against the same cases.

### Cache

- `media.ambar.page` is proxied by Cloudflare to the VPS. One cache rule makes `/previews/` eligible for cache. The cache key is the **full URL, signature included** (the default), so a cache hit always needs a valid signed URL.
- The API answers `Cache-Control: public, max-age=N, s-maxage=N`, where `N` is the seconds left until `exp`. No cached copy outlives its URL. Serving stale content (stale-while-revalidate, Always Online) is turned off for this host.
- Every preview carries `Cache-Tag: m-{momentId}`, so erasing a moment purges every cached copy at once.
- Clients store moment ids, never media URLs. An expired URL is renewed by reading the moment again (`getMoment` or `listMoments`).

### Never as a page

Every preview response carries:
- the `Content-Type` that processing verified (`image/webp`);
- `X-Content-Type-Options: nosniff`;
- `Content-Security-Policy: sandbox; default-src 'none'`;
- `Cross-Origin-Resource-Policy: same-site`.

Originals come from R2's own domain, a different site from `ambar.page`, with the verified `Content-Type` stored on the object. `?download=true` adds `Content-Disposition: attachment` with the original name. SVG and HTML are never accepted.

### One domain for now

`media.ambar.page` shares a registrable domain with the app and the API. Two things keep that safe:
- the session cookie is `__Host-`, so a subdomain can't plant or read it;
- nothing served from it can run as a page.

### Rotating credentials

No credential encrypts data. R2 encrypts at rest with keys Cloudflare manages, so rotating any of ours never makes a file unreadable.

| Credential | What rotating it does | How |
| --- | --- | --- |
| `MEDIA_SIGNING_KEY` | Preview URLs signed with the old key stop working; clients read the moments again and get new ones | Add the new key under a new `kid` and sign with it. Keep verifying the old one for 36 hours, or drop it at once in an emergency |
| R2 access key | The API uses the new one; nothing changes for files or URLs already issued, except originals presigned with the old key (15 minutes) | Create a new token, deploy it, revoke the old one |
| Cache purge token | Nothing visible | Same as the R2 access key |

The Cloudflare account itself is the one access that can't be replaced by rotating. Protect it with hardware-key 2FA and recovery codes kept offline. The backup bucket (later) protects against losing the files themselves.

### Moving to a Worker later

When edge verification or global latency is worth it, `media.ambar.page` moves from the VPS to a Worker with an R2 binding. Clients don't change, because the API keeps generating the URLs:
- the Worker verifies the same signed string and `kid`, so URLs issued before the switch keep working;
- it reproduces the same headers, checked by the same tests;
- the key then also lives in Cloudflare, or the API moves to Ed25519 under a new `kid` and the Worker holds only the public key;
- the first views after the switch are cache misses, not errors.

Files stay at the same keys in R2, and permissions stay in Postgres. Nothing a user keeps depends on where previews are served from.

## Who can see what

- Everything belongs to a vault. A person's library is their own vault.
- Access is decided by `vault_members` (`owner`, `editor`, `viewer`), for people and agents alike.
- Sharing a vault adds a member. Sharing one moment or a collection later adds a `shares` table (token, scope, expiry). Storage and the media URLs don't change, because the API still decides and signs.
- Removing a member stops new URLs at once. Ones already issued work until they expire: previews up to 36 hours, originals 15 minutes. Rotating `MEDIA_SIGNING_KEY` ends the previews at once when that matters.

## Deleting

Moving a moment to the Trash changes nothing in R2. Erasing it for good (after `TRASH_DAYS`, or Empty Trash) does four things:
- deletes its original and every variant;
- deletes any leftover in `incoming/`;
- deletes its readings and descriptions;
- purges its cached previews by tag (`m-{momentId}`).

If a purge fails, a cached copy can still be served only to someone holding a URL signed before the erase, and only until it expires.

## Where data lives

| Data | Where |
| --- | --- |
| Files (originals and variants) | R2 bucket with EU jurisdiction planned; verify configuration and provider terms before making a residency claim |
| Records, readings, descriptions | Postgres on the VPS |
| Media delivery | Previews through Cloudflare's CDN cache; originals from R2. Edge location is not guaranteed to be in the EU |
| Readings and AI processing | Not built; no provider chosen. ZDR is required; verify region and data-use terms before enabling a provider |

Public copy (privacy policy, landing) states only what has been checked against the providers' terms and the running configuration.

## Verify while building

Before depending on provider-specific behavior, test it from every supported app origin and against the exact account, bucket and SDK configuration used by that environment. Keep operational observations and resource identifiers in local deployment notes; version only the resulting requirements and repeatable checks.

- A signed upload must be rejected if its actual type, size or SHA-256 differs from the reservation, including a second `PUT` to the same URL after the first succeeded. The API must still verify the bytes itself; provider signature checks are an additional boundary.
- Expired upload URLs must fail, and the client must keep the local file and request a fresh slot.
- CORS must allow only the app origins and methods required by the flow, and expose only headers the client reads.
- Completion must compare the stored object's actual size, type and checksum with the reservation, and refuse headers a client added that change how the file is read (`Content-Encoding`, `Content-Disposition`, `Content-Language`, `Expires`, metadata). Browsers add `Cache-Control` on their own, so completion accepts it and every read from storage must set its own.
- Presigned downloads must use the expected filename and response type.
- Verify the region-specific endpoint, SDK checksum behavior, signed-URL expiry and cache-purge behavior before enabling the environment.

The current environment's command results and resource names are kept in ignored `docs/local/` notes.

## Failure modes to design for

| Failure | Expected behavior in the design | Verification or operational requirement |
| --- | --- | --- |
| Upload URL expires or browser CORS rejects the request | Keep the file locally and request a fresh slot when safe; do not mark it as stored | Test preflight and expired URLs from each app origin in supported browsers |
| Client closes before completion | Expire the reservation and remove the incoming object later | Cleanup and the R2 lifecycle rule must be independently retryable |
| Object size, type or fingerprint differs from the request | Reject processing, delete the incoming object and release the reservation | Re-read bytes server-side; test size enforcement before signing so a false size cannot bypass the upload limit or consume unbounded storage |
| Processing stops after completion | Resume idempotently or transition to a visible failed state | Define retry limits, failure codes and how operators can inspect stuck uploads |
| Vault access is revoked after a media URL is issued | Previously issued URLs remain usable until they expire (previews 36 hours at most, originals 15 minutes) | Document the maximum exposure window; deletion should also attempt cache purge |
| Cache purge fails after permanent deletion | A holder of an unexpired signed URL may still read the cached object | Alert on purge failures and rely only on the bounded URL expiry as fallback |
| AI provider or policy cannot meet ZDR requirements | Do not send user content to that provider | Verify ZDR, region, training, subprocessors and logging before enabling a provider |

## Target architecture and open decisions

- The supplied diagram shows Cloudflare Queues and a Go queue consumer. Decision 0006 proposes no external queue for the first implementation and leaves Cloudflare Queues versus River open. Treat the diagram's queue path as a target option until a decision supersedes the ADR.
- **Multipart uploads** for files above 100 MB.
- **A Worker for previews**, when edge verification or global latency justifies its per-request cost (see Moving to a Worker later).
- **A separate media domain.**
- **Perceptual hashes** for near duplicates.
- Agent API keys, MCP access, SSE, vector search, replicas and backups also appear in the target diagram, but have no implementation or accepted API contract yet.
