package main

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"ambar/api/storage"
	"ambar/api/uploads"
	"ambar/api/vaults"
)

// An upload request lists up to uploads.MaxFiles files with their names and fingerprints. The largest a
// correct client can send (255-character names with every character JSON-escaped as a surrogate pair,
// 12 bytes each) is about 66 KB: TestUploadsTakeTheLargestValidRequest.
const maxUploadBodyBytes = 128 << 10

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// refusedCopy is the end-user sentence for each per-file refusal and failure.
var refusedCopy = map[string]string{
	uploads.UnsupportedType: "Ambar keeps photos, clips and PDFs.",
	uploads.FileTooLarge:    "This file is bigger than 100 MB.",
	uploads.QuotaExceeded:   "There isn't enough room left in this library.",
	uploads.Mismatch:        "The file changed while it was uploading. Try keeping it again.",
}

type uploadJSON struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	Name        string       `json:"name"`
	Size        int64        `json:"size"`
	ContentType string       `json:"contentType"`
	CreatedAt   time.Time    `json:"createdAt"`
	ExpiresAt   time.Time    `json:"expiresAt"`
	Failure     *failureJSON `json:"failure,omitempty"`
}

type failureJSON struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func uploadToJSON(u uploads.Upload) uploadJSON {
	out := uploadJSON{ID: u.ID, Status: u.Status, Name: u.Name, Size: u.Size, ContentType: u.ContentType, CreatedAt: u.CreatedAt, ExpiresAt: u.ExpiresAt}

	if u.FailureCode != "" {
		out.Failure = &failureJSON{Code: u.FailureCode, Detail: refusedCopy[u.FailureCode]}
	}

	return out
}

// POST /v1/uploads {vaultId?, files} → 201 {results, quota}
func (a *api) handleCreateUploads(w http.ResponseWriter, r *http.Request) {
	if a.uploads == nil {
		storageOff(w, r)

		return
	}

	var body struct {
		VaultID string `json:"vaultId"`
		Files   []struct {
			Name        string     `json:"name"`
			Size        int64      `json:"size"`
			ContentType string     `json:"contentType"`
			SHA256      string     `json:"sha256"`
			CapturedAt  *time.Time `json:"capturedAt"`
			Width       *int       `json:"width"`
			Height      *int       `json:"height"`
			DurationMs  *int       `json:"durationMs"`
		} `json:"files"`
	}
	if !decodeJSONUpTo(w, r, &body, maxUploadBodyBytes) {
		return
	}

	if body.VaultID != "" && !uuidPattern.MatchString(body.VaultID) {
		a.uploadError(w, r, vaults.ErrNotFound)

		return
	}

	files := make([]uploads.File, len(body.Files))
	for i, f := range body.Files {
		files[i] = uploads.File{Name: f.Name, Size: f.Size, ContentType: f.ContentType, SHA256: f.SHA256, CapturedAt: f.CapturedAt, Width: f.Width, Height: f.Height, DurationMs: f.DurationMs}
	}

	session := sessionFrom(r.Context())

	results, quota, err := a.uploads.Create(r.Context(), session.UserID, body.VaultID, files)
	if err != nil {
		a.uploadError(w, r, err)

		return
	}

	out := make([]map[string]any, len(results))
	accepted, reserved := 0, int64(0)

	for i, result := range results {
		if result.Upload == nil {
			refused := map[string]any{"status": "rejected", "code": result.Refused, "detail": refusedCopy[result.Refused]}

			switch result.Refused {
			case uploads.FileTooLarge:
				refused["maxBytes"] = result.MaxBytes
			case uploads.QuotaExceeded:
				refused["bytesLeft"] = result.BytesLeft
			}

			out[i] = refused

			continue
		}

		accepted++
		reserved += result.Upload.Size
		out[i] = map[string]any{
			"status": "accepted",
			"upload": uploadToJSON(*result.Upload),
			"put":    map[string]any{"method": "put", "url": result.Put.URL, "headers": result.Put.Headers, "expiresAt": result.Put.ExpiresAt},
		}
	}

	slog.InfoContext(r.Context(), "uploads reserved", "event", "upload.reserved", "user_id", session.UserID, "accepted", accepted, "refused", len(results)-accepted, "bytes", reserved)

	writeJSON(w, http.StatusCreated, map[string]any{
		"results": out,
		"quota":   map[string]any{"usedBytes": quota.Used, "reservedBytes": quota.Reserved, "limitBytes": quota.Limit},
	})
}

// GET /v1/uploads/{uploadId} → 200 upload
func (a *api) handleGetUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := a.uploadID(w, r)
	if !ok {
		return
	}

	upload, err := a.uploads.Get(r.Context(), sessionFrom(r.Context()).UserID, id)
	if err != nil {
		a.uploadError(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, uploadToJSON(upload))
}

// POST /v1/uploads/{uploadId}:complete → 202 upload
func (a *api) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := a.uploadID(w, r)
	if !ok {
		return
	}

	upload, err := a.uploads.Complete(r.Context(), sessionFrom(r.Context()).UserID, id)
	if err != nil {
		a.uploadError(w, r, err)

		return
	}

	switch upload.Status {
	case uploads.Uploaded:
		slog.InfoContext(r.Context(), "upload completed", "event", "upload.completed", "upload_id", upload.ID, "bytes", upload.Size)
	case uploads.Failed:
		slog.WarnContext(r.Context(), "upload failed", "event", "upload.failed", "upload_id", upload.ID, "code", upload.FailureCode)
	}

	writeJSON(w, http.StatusAccepted, uploadToJSON(upload))
}

// DELETE /v1/uploads/{uploadId} → 204
func (a *api) handleCancelUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := a.uploadID(w, r)
	if !ok {
		return
	}

	if err := a.uploads.Cancel(r.Context(), sessionFrom(r.Context()).UserID, id); err != nil {
		a.uploadError(w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// uploadID reads the path id. With storage off, or an id that can't be an upload's, it answers already.
func (a *api) uploadID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if a.uploads == nil {
		storageOff(w, r)

		return "", false
	}

	id := chi.URLParam(r, "uploadId")
	if !uuidPattern.MatchString(id) {
		a.uploadError(w, r, uploads.ErrNotFound)

		return "", false
	}

	return id, true
}

// storageOff: development without R2 configured (production refuses to start that way).
func storageOff(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "30")
	writeProblem(w, r, http.StatusServiceUnavailable, "storage-unavailable", "We can't reach storage right now. Try again in a moment.")
}

func (a *api) uploadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, uploads.ErrInvalid):
		writeProblem(w, r, http.StatusBadRequest, "bad-request", "That request doesn't look right.")
	case errors.Is(err, vaults.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "vault-not-found", "We couldn't find that library.")
	case errors.Is(err, uploads.ErrReadOnly):
		writeProblem(w, r, http.StatusForbidden, "vault-read-only", "You can view this library, but not add to it.")
	case errors.Is(err, uploads.ErrTooManyPending):
		w.Header().Set("Retry-After", "60")
		writeProblemWith(w, r, http.StatusTooManyRequests, "rate-limited", "Too many uploads in progress. Let them finish first.", map[string]any{"retryAfterMs": 60_000})
	case errors.Is(err, uploads.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "upload-not-found", "We couldn't find that upload.")
	case errors.Is(err, uploads.ErrNotPending):
		writeProblem(w, r, http.StatusConflict, "upload-not-pending", "That upload is already being kept.")
	case errors.Is(err, uploads.ErrMissing):
		writeProblem(w, r, http.StatusConflict, "upload-missing", "The file hasn't finished uploading. Try again.")
	case errors.Is(err, uploads.ErrExpired):
		writeProblem(w, r, http.StatusGone, "upload-expired", "That upload took too long. Try keeping the file again.")
	case errors.Is(err, storage.ErrUnavailable):
		slog.ErrorContext(r.Context(), "storage unavailable", "event", "upload.storage_failed")
		storageOff(w, r)
	default:
		slog.ErrorContext(r.Context(), "uploads", "event", "upload.error", "err", err)
		writeProblem(w, r, http.StatusInternalServerError, "internal", "Something went wrong on our side.")
	}
}
