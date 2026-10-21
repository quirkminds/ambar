# 0006. Media storage on R2, uploads signed by the API

- Status: accepted
- Date: 2026-10-21

## Context

Photos, clips and PDFs are by far the heaviest thing Ambar handles. If their bytes went through the API on the VPS, one person uploading a clip would hold its network and memory for seconds and slow everyone else down. Media must also stay private: a copied image URL must not show someone's capture forever, and the files carry personal metadata (GPS, device, author).

This follows Cloudflare's [storing user-generated content](https://developers.cloudflare.com/reference-architecture/diagrams/storage/storing-user-generated-content/) reference architecture, with the API (Go) where it puts a Worker and Postgres where it puts D1. The full design, with the problems found while reviewing it, is in [explanation/media-storage.md](../explanation/media-storage.md).

The architecture diagram in [the system overview](../explanation/overview.md) is a broader target. It shows an external queue and AI processing; this ADR does not approve those components or change the current Go API, which does not serve media uploads.

## Decision

- **Uploads go from the client straight to R2.** The API checks the session, the vault permission, the quota, the size and the type, reserves the bytes and answers with one presigned `PUT` per file (minutes, one key). Bind the expected type and size to the upload and verify the browser/R2 signing behavior before fixing the request headers in the contract. The client calls `:complete` when the `PUT` succeeds; the API processes the file in the background.
- **No queue yet.** Processing runs in the API process, picking work from Postgres (`FOR UPDATE SKIP LOCKED`), so a restart resumes it. Cloudflare Queues (R2 event notifications, HTTP pull consumer) or River come in when the load asks for them.
- **One private bucket, EU jurisdiction planned**, with no public bucket or `r2.dev`. Verify the configured jurisdiction and provider terms before claiming data residency. Keys are ids only: `incoming/{uploadId}` until processed, then `vaults/{vaultId}/moments/{momentId}/{variant}`. The original file name lives in Postgres.
- **Metadata stripped without re-encoding.** The client extracts capture date and dimensions, sends them as declared values and strips the rest before uploading (lossless, byte-level for JPEG). The API strips again as the guarantee, keeps orientation, and uses libvips only for derived sizes.
- **Fingerprints dedupe inside a vault only.** SHA-256 of the uploaded bytes, declared by the client and recomputed by the API. Never compared across vaults.
- **The API serves previews, Cloudflare caches them.** Previews (480 and 1200 px) live at `media.ambar.page`, proxied by Cloudflare to the API, which checks an HMAC-SHA256 signature (its own `MEDIA_SIGNING_KEY`, with a key id) and streams from R2. Expiry is rounded to 24-hour windows, so a URL stays the same while it is valid. The CDN caches under the full signed URL, never past `exp`, at no per-request cost. Originals (photos, clips, PDFs) are redirects to R2 presigned `GET`s valid for 15 minutes. No Worker yet. Responses never run as a page: verified `Content-Type`, `nosniff`, a sandboxing CSP.
- **Access is by vault membership** (`owner`, `editor`, `viewer`), for people and agents alike. Shared items or folders later add a `shares` table; the media path stays the same.
- **AI processing has a ZDR gate.** Ambar only selects providers that meet the zero-data-retention requirement. Region, training use, subprocessors, gateway logs and applicable compliance obligations require separate verification before user content is sent; this ADR does not claim those guarantees are already met.

## Consequences

- The API spends milliseconds per upload deciding and signing. Bytes travel Cloudflare's network, with no egress fees.
- Someone removed from a vault keeps seeing previews they already have URLs for until those expire (at most 36 hours; originals 15 minutes). Rotating the signing key ends that at once.
- R2 bills storage and operations, never egress. A preview cache hit costs nothing; a miss costs one R2 read and VPS bandwidth; an original costs one R2 read.
- Moving previews to a Worker later changes DNS and the verifier, not the clients or the stored files.
- Public copy about where data lives states only what has been checked against the providers' terms and the running configuration.
- One registrable domain for now. Media URLs are generated per response and expire, so moving media to its own domain later is a configuration change.
- The VPS needs libvips and ffmpeg. Single `PUT`s cap files at the current 100 MB; larger files need multipart, which the contract leaves room for.

## Options considered

- **Upload through the API:** simplest, but every byte occupies the VPS.
- **R2's own presigned GET URLs for viewing:** served from R2's domain, so there is no CDN cache. They also change on every signing.
- **Check the session cookie on every image:** a Postgres query per thumbnail, and no edge cache.
- **A Worker in front of R2 from the start:** verification at the edge, but every view is a billed Worker request, cache hits included, and its Cache API is per data center (no Tiered Cache).
- **R2 custom domain with WAF token authentication:** no code and Tiered Cache, but it needs the Pro plan and Cloudflare's fixed token format.
- **Queues from day one:** survives a closed tab and absorbs bursts, but it's another moving part before there is load to justify it.
