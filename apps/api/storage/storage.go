// Package storage is the API's side of R2, through its S3 API: presigning the uploads clients send
// straight to the bucket, and reading or deleting what is there. The bytes never pass through the API.
// R2's errors are mapped to ours and never carry the provider's text.
package storage

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// callBudget bounds each call to R2, the SDK's retries included (only HEAD and DELETE go out; both are
// idempotent).
const callBudget = 10 * time.Second

var (
	ErrNotFound    = errors.New("object not found")
	ErrUnavailable = errors.New("storage unavailable")
)

// Object is what a HEAD tells about a stored file. SHA256 (hex) is the checksum R2 verified on upload.
// Unsigned names the headers stored with the object that PresignPut never signs: R2 keeps standard ones
// a client adds to a signed PUT (Content-Encoding, Content-Disposition, …), so they must be refused later.
type Object struct {
	Size        int64
	ContentType string
	SHA256      string
	Unsigned    []string
}

type Client struct {
	s3      *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// New connects to one bucket. endpoint is the account's S3 endpoint for the bucket's jurisdiction
// (https://<account>.eu.r2.cloudflarestorage.com for an EU bucket).
func New(endpoint, accessKeyID, secretAccessKey, bucket string) *Client {
	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
		// R2 doesn't take the checksums the SDK adds by default to presigned requests.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})

	return &Client{s3: client, presign: s3.NewPresignClient(client), bucket: bucket}
}

// PresignPut signs one PUT to key, valid for ttl, of exactly the declared bytes: their size, type and
// SHA-256 (hex) are all signed, and R2 refuses any other body, so the URL can't be reused to swap the
// file after Complete checked it. headers are the ones the client must send; Content-Length is signed
// too, but browsers set it from the body themselves.
func (c *Client) PresignPut(ctx context.Context, key, contentType string, size int64, sha256Hex string, ttl time.Duration) (url string, headers map[string]string, err error) {
	sum, err := hex.DecodeString(sha256Hex)
	if err != nil || len(sum) != 32 {
		return "", nil, errors.New("storage: sha256 must be 64 hex characters")
	}

	checksum := base64.StdEncoding.EncodeToString(sum)

	req, err := c.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:         &c.bucket,
		Key:            &key,
		ContentType:    &contentType,
		ContentLength:  &size,
		ChecksumSHA256: &checksum,
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", nil, ErrUnavailable
	}

	headers = map[string]string{}

	for name, values := range req.SignedHeader {
		switch strings.ToLower(name) {
		case "host", "content-length":
			// Set by every HTTP client from the URL and the body.
		default:
			headers[name] = values[0]
		}
	}

	return req.URL, headers, nil
}

// Head reads a stored object's size and type.
func (c *Client) Head(ctx context.Context, key string) (Object, error) {
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()

	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &key, ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return Object{}, mapError(err)
	}

	object := Object{Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType)}

	for name, value := range map[string]*string{
		"Cache-Control":       out.CacheControl,
		"Content-Disposition": out.ContentDisposition,
		"Content-Encoding":    out.ContentEncoding,
		"Content-Language":    out.ContentLanguage,
		"Expires":             out.ExpiresString,
	} {
		if aws.ToString(value) != "" {
			object.Unsigned = append(object.Unsigned, name)
		}
	}

	if len(out.Metadata) > 0 {
		object.Unsigned = append(object.Unsigned, "x-amz-meta-*")
	}

	if sum, err := base64.StdEncoding.DecodeString(aws.ToString(out.ChecksumSHA256)); err == nil && len(sum) == 32 {
		object.SHA256 = hex.EncodeToString(sum)
	}

	return object, nil
}

// Delete removes an object. Deleting one that isn't there succeeds.
func (c *Client) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()

	if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &c.bucket, Key: &key}); err != nil {
		return mapError(err)
	}

	return nil
}

// mapError keeps R2's message out of our errors: it can echo keys and request details.
func mapError(err error) error {
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound {
		return ErrNotFound
	}

	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey") {
		return ErrNotFound
	}

	return ErrUnavailable
}
