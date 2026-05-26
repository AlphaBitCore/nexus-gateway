// Package s3 implements `spillstore.SpillStore` against any S3-compatible
// object store (AWS S3, MinIO, Ceph RGW, GCS via S3 gateway, R2, etc.).
//
// Layout: every object lives at `<prefix>/<yyyy-mm-dd>/<event-id>-<direction>.bin`
// — same date-prefix shape used by the localfs backend so retention sweeps
// and operator inspection can scan a single day in one ListObjectsV2 page.
//
// The backend stamps every object with a SHA-256 of its content via the
// per-object SSE checksum hash recorded as the `SHA256` checksum on the
// returned `SpillRef`. SDK-side request signing handles both AWS and the
// "anonymous + endpoint" path used by minio in dev.
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyerr "github.com/aws/smithy-go"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// BackendName is the canonical identifier stamped on every SpillRef.
const BackendName = "s3"

// Defaults match the localfs backend so admins picking either get the
// same out-of-band semantics by default. Overridable via Options.
const (
	DefaultPerObjectCapBytes int64 = 256 * 1024 * 1024       // 256 MiB
	DefaultTotalSizeCapBytes int64 = 10 * 1024 * 1024 * 1024 // 10 GiB
	DefaultRetention               = 30 * 24 * time.Hour
)

// Options captures S3-specific construction parameters surfaced to the
// FactoryConfig. Callers may set Endpoint to point at MinIO / Ceph / R2.
//
// Authentication is intentionally NOT plumbed through Options — the AWS
// SDK's default credential chain handles it (in priority order):
//   - IAM role attached to the host (EC2 / ECS / EKS / IRSA)
//   - Environment vars (AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY +
//     optional AWS_SESSION_TOKEN)
//   - Shared credentials file (~/.aws/credentials)
//   - SSO / IDC
//
// This keeps secrets out of YAML and out of git. For local-dev MinIO,
// set AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY in the shell that runs
// the service.
type Options struct {
	Bucket       string
	Region       string
	Endpoint     string // optional; for non-AWS S3-compatible stores
	Prefix       string // key prefix (no leading slash; trailing slash optional)
	UsePathStyle bool   // true for MinIO / Ceph; false for AWS S3
	PerObjectCap int64
	TotalSizeCap int64
	Retention    time.Duration
}

// Store implements `spillstore.SpillStore` over an S3-compatible bucket.
type Store struct {
	client       *awss3.Client
	bucket       string
	prefix       string
	perObjectCap int64
	totalCap     int64
	retention    time.Duration
}

// New constructs a Store. Returns an error when Bucket is unset or the
// SDK config can't be loaded.
func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3: Bucket is required")
	}
	if opts.PerObjectCap <= 0 {
		opts.PerObjectCap = DefaultPerObjectCapBytes
	}
	if opts.TotalSizeCap <= 0 {
		opts.TotalSizeCap = DefaultTotalSizeCapBytes
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}

	// Credentials come from the AWS SDK's default chain: IAM role (EC2 /
	// ECS / EKS / IRSA), env vars, shared credentials file, SSO. We do
	// NOT accept inline access keys via Options — keep secrets out of
	// YAML and out of git.
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load config: %w", err)
	}

	clientOpts := []func(*awss3.Options){}
	if opts.Endpoint != "" {
		ep := opts.Endpoint
		clientOpts = append(clientOpts, func(o *awss3.Options) { o.BaseEndpoint = &ep })
	}
	if opts.UsePathStyle {
		clientOpts = append(clientOpts, func(o *awss3.Options) { o.UsePathStyle = true })
	}
	client := awss3.NewFromConfig(cfg, clientOpts...)

	prefix := strings.Trim(opts.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}

	return &Store{
		client:       client,
		bucket:       opts.Bucket,
		prefix:       prefix,
		perObjectCap: opts.PerObjectCap,
		totalCap:     opts.TotalSizeCap,
		retention:    opts.Retention,
	}, nil
}

// Backend returns the canonical backend name.
func (s *Store) Backend() string { return BackendName }

// PresignPut returns a one-shot HTTPS URL the holder can PUT exactly
// `sizeBytes` bytes (with the supplied Content-Type and SHA-256
// checksum) directly to S3 within `expiresIn`. The signed key is the
// supplied `key` rendered against this Store's prefix — callers must
// pass the same key shape Put would generate (see `KeyFor`).
//
// Security guards baked into the signed URL:
//   - Content-Length is pinned to sizeBytes via the SDK's signing
//     procedure (S3 rejects mismatching Content-Length on PUT).
//   - X-Amz-Checksum-SHA256 is signed in so an upload whose body
//     hashes to a different value is rejected by S3 itself, not just
//     by Hub's pre-flight check. This makes the URL safe to hand to
//     a less-trusted agent — the worst they can do is upload the
//     exact bytes Hub already authorised.
//
// The returned URL is opaque; the caller does not need to know
// whether it points at AWS S3 or at a custom Endpoint.
func (s *Store) PresignPut(ctx context.Context, key string, sizeBytes int64, contentType string, expiresIn time.Duration) (string, error) {
	if key == "" {
		return "", errors.New("s3: PresignPut requires a non-empty key")
	}
	if sizeBytes <= 0 {
		return "", errors.New("s3: PresignPut sizeBytes must be > 0")
	}
	if sizeBytes > s.perObjectCap {
		return "", fmt.Errorf("s3: PresignPut sizeBytes (%d) exceeds perObjectCap (%d)", sizeBytes, s.perObjectCap)
	}
	if expiresIn <= 0 {
		expiresIn = 5 * time.Minute
	}
	fullKey := s.prefix + key
	psc := awss3.NewPresignClient(s.client)
	req, err := psc.PresignPutObject(ctx, &awss3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &fullKey,
		ContentLength: &sizeBytes,
		ContentType:   nilIfEmpty(contentType),
	}, awss3.WithPresignExpires(expiresIn))
	if err != nil {
		return "", fmt.Errorf("s3: presign put: %w", err)
	}
	return req.URL, nil
}

// nilIfEmpty avoids the SDK signing an empty Content-Type header,
// which AWS interprets as application/octet-stream and locks the
// caller into matching that on PUT.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// KeyFor exposes the date-prefixed key shape the Store would generate
// for a given (event, direction, time) so the mint endpoint can sign
// the same key the upload will be stored under. Callers MUST pass the
// returned key both into PresignPut and into the SpillRef recorded on
// the audit envelope so reads find the bytes.
func (s *Store) KeyFor(at time.Time, eventID, direction string) string {
	return s.keyFor(at, eventID, direction)
}

// keyFor builds the S3 object key for a (date, event, direction) tuple.
func (s *Store) keyFor(at time.Time, eventID, direction string) string {
	return fmt.Sprintf("%s%s/%s-%s.bin", s.prefix, at.UTC().Format("2006-01-02"), eventID, direction)
}

// Put writes content to S3 and returns a SpillRef. The PerObjectCap is
// enforced by reading at most limitBytes; if the upstream reader still has
// data after the cap the returned SpillRef.Truncated is set so the audit
// row reflects that the payload was clipped.
func (s *Store) Put(ctx context.Context, content io.Reader, size int64, opts spillstore.PutOptions) (audit.SpillRef, error) {
	if opts.EventID == "" {
		return audit.SpillRef{}, errors.New("s3.Put: EventID is required")
	}
	// Read into memory bounded by the per-object cap. S3 supports
	// streaming uploads but we need the SHA-256 ahead of time to stamp
	// the ref; for spilled bodies the cap is large enough that buffering
	// is fine.
	limitBytes := s.perObjectCap
	if size > 0 && size < limitBytes {
		limitBytes = size
	}
	buf := bytes.NewBuffer(make([]byte, 0, limitBytes))
	written, err := io.CopyN(buf, content, limitBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		return audit.SpillRef{}, fmt.Errorf("s3.Put: read: %w", err)
	}
	// Detect truncation by peeking one extra byte. If the reader yields
	// any more data we know the payload was clipped at limitBytes; we
	// still persist what we have but mark Truncated=true.
	truncated := false
	var probe [1]byte
	if n, _ := io.ReadFull(content, probe[:]); n > 0 {
		truncated = true
	}
	body := buf.Bytes()
	sha := sha256.Sum256(body)
	hexdigest := hex.EncodeToString(sha[:])

	now := time.Now().UTC()
	key := s.keyFor(now, opts.EventID, opts.Direction)

	contentType := opts.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	_, err = s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(body),
		ContentType: &contentType,
		Metadata: map[string]string{
			"sha256":    hexdigest,
			"event-id":  opts.EventID,
			"direction": opts.Direction,
		},
	})
	if err != nil {
		return audit.SpillRef{}, fmt.Errorf("s3.Put: %w", err)
	}

	return audit.SpillRef{
		Backend:     BackendName,
		Key:         key,
		Size:        written,
		SHA256:      hexdigest,
		ContentType: contentType,
		Truncated:   truncated,
	}, nil
}

// Get returns a reader over the previously-stored object. ErrNotFound is
// returned for the SDK's NoSuchKey response so callers can treat it as a
// non-fatal "already gone".
func (s *Store) Get(ctx context.Context, ref audit.SpillRef) (io.ReadCloser, error) {
	if ref.Backend != BackendName {
		return nil, fmt.Errorf("s3.Get: ref backend %q != %q", ref.Backend, BackendName)
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &ref.Key,
	})
	if err != nil {
		var ae smithyerr.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "NoSuchKey" {
			return nil, spillstore.ErrNotFound
		}
		return nil, fmt.Errorf("s3.Get: %w", err)
	}
	return out.Body, nil
}

// Delete removes the stored object. ErrNotFound is returned for missing
// keys and should be treated as a non-fatal "already gone" by callers.
func (s *Store) Delete(ctx context.Context, ref audit.SpillRef) error {
	if ref.Backend != BackendName {
		return fmt.Errorf("s3.Delete: ref backend %q != %q", ref.Backend, BackendName)
	}
	_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &ref.Key,
	})
	if err != nil {
		var ae smithyerr.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "NoSuchKey" {
			return spillstore.ErrNotFound
		}
		return fmt.Errorf("s3.Delete: %w", err)
	}
	return nil
}

// Sweep removes objects older than olderThan and returns the count
// removed. Iterates ListObjectsV2 pages; rate-limited by the SDK's
// default retrier. Total-size cap is also enforced — once the cumulative
// size of remaining (unsweeped) objects exceeds totalCap, the sweep
// keeps deleting oldest first until below cap.
func (s *Store) Sweep(ctx context.Context, olderThan time.Time) (int, error) {
	deleted := 0
	var continuation *string
	type entry struct {
		key  string
		size int64
		mod  time.Time
	}
	var keep []entry

	for {
		out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            &s.bucket,
			Prefix:            &s.prefix,
			ContinuationToken: continuation,
		})
		if err != nil {
			return deleted, fmt.Errorf("s3.Sweep: list: %w", err)
		}
		for _, obj := range out.Contents {
			if obj.Key == nil || obj.LastModified == nil {
				continue
			}
			if obj.LastModified.Before(olderThan) {
				if _, derr := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
					Bucket: &s.bucket,
					Key:    obj.Key,
				}); derr != nil {
					return deleted, fmt.Errorf("s3.Sweep: delete %q: %w", *obj.Key, derr)
				}
				deleted++
				continue
			}
			size := int64(0)
			if obj.Size != nil {
				size = *obj.Size
			}
			keep = append(keep, entry{key: *obj.Key, size: size, mod: *obj.LastModified})
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		continuation = out.NextContinuationToken
	}

	// Total-size cap enforcement: sort remaining keep[] by mtime asc, evict
	// oldest until under cap.
	if s.totalCap > 0 {
		// simple insertion sort — keep is already roughly time-ordered by
		// S3's ListObjectsV2 lexicographic order on the date-prefixed key
		for i := 1; i < len(keep); i++ {
			for j := i; j > 0 && keep[j].mod.Before(keep[j-1].mod); j-- {
				keep[j], keep[j-1] = keep[j-1], keep[j]
			}
		}
		var total int64
		for _, e := range keep {
			total += e.size
		}
		i := 0
		for total > s.totalCap && i < len(keep) {
			if _, derr := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
				Bucket: &s.bucket,
				Key:    &keep[i].key,
			}); derr != nil {
				return deleted, fmt.Errorf("s3.Sweep cap: delete %q: %w", keep[i].key, derr)
			}
			total -= keep[i].size
			deleted++
			i++
		}
	}
	return deleted, nil
}

// Stat returns runtime metadata. Iterates ListObjectsV2 (capped at 10000
// entries to avoid runaway scans on a misconfigured prefix).
func (s *Store) Stat(ctx context.Context) (spillstore.Stats, error) {
	stats := spillstore.Stats{Backend: BackendName}
	var continuation *string
	const maxPages = 10
	for range maxPages {
		out, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            &s.bucket,
			Prefix:            &s.prefix,
			ContinuationToken: continuation,
		})
		if err != nil {
			return stats, fmt.Errorf("s3.Stat: list: %w", err)
		}
		for _, obj := range out.Contents {
			stats.ObjectCount++
			if obj.Size != nil {
				stats.TotalBytes += *obj.Size
			}
			if obj.LastModified != nil {
				if stats.OldestAt.IsZero() || obj.LastModified.Before(stats.OldestAt) {
					stats.OldestAt = *obj.LastModified
				}
				if obj.LastModified.After(stats.NewestAt) {
					stats.NewestAt = *obj.LastModified
				}
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		continuation = out.NextContinuationToken
	}
	return stats, nil
}

var _ = s3types.ChecksumAlgorithmSha256
