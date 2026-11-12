// Package objstore holds the S3 plumbing shared by the plan store and the job
// log archive: client construction, key layout, encryption, paginated
// listing, atomic downloads, and prefix deletion.
//
// On Kubernetes, prefer workload identity (IRSA on EKS, or an S3-compatible
// endpoint's equivalent) over static keys; the default AWS credential chain
// picks it up without configuration.
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// OpTimeout bounds a single S3 call.
const OpTimeout = 30 * time.Second

// API is the subset of the S3 client used by Atlantis.
type API interface {
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

var _ API = (*s3.Client)(nil)

// Config describes one bucket.
type Config struct {
	Bucket         string
	Region         string
	Prefix         string
	Endpoint       string
	ForcePathStyle bool
	Profile        string
	// ServerSideEncryption is AES256, aws:kms or aws:kms:dsse; empty uses the
	// bucket default.
	ServerSideEncryption string
	KMSKeyID             string
}

// NewS3 builds a client for cfg and checks that the bucket is reachable.
func NewS3(ctx context.Context, cfg Config) (*s3.Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	})
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		return nil, fmt.Errorf("checking bucket %q: %w", cfg.Bucket, err)
	}
	return client, nil
}

// Bucket is a bucket plus the key prefix and write options for one store.
type Bucket struct {
	Client API
	Name   string
	// Prefix is prepended to every key; it has no trailing slash.
	Prefix     string
	Encryption types.ServerSideEncryption
	KMSKeyID   string
}

// NewBucket returns a Bucket for cfg using client.
func NewBucket(client API, cfg Config) *Bucket {
	return &Bucket{
		Client:     client,
		Name:       cfg.Bucket,
		Prefix:     strings.Trim(cfg.Prefix, "/"),
		Encryption: types.ServerSideEncryption(cfg.ServerSideEncryption),
		KMSKeyID:   cfg.KMSKeyID,
	}
}

// Key joins parts under the bucket prefix, skipping empty parts.
func (b *Bucket) Key(parts ...string) string {
	all := make([]string, 0, len(parts)+1)
	if b.Prefix != "" {
		all = append(all, b.Prefix)
	}
	for _, p := range parts {
		if p = strings.Trim(p, "/"); p != "" {
			all = append(all, p)
		}
	}
	return strings.Join(all, "/")
}

// Put uploads body to key with the bucket's encryption settings.
func (b *Bucket) Put(ctx context.Context, key string, body io.Reader, metadata map[string]string) error {
	in := &s3.PutObjectInput{
		Bucket:   aws.String(b.Name),
		Key:      aws.String(key),
		Body:     body,
		Metadata: metadata,
	}
	if b.Encryption != "" {
		in.ServerSideEncryption = b.Encryption
	}
	if b.KMSKeyID != "" {
		in.SSEKMSKeyId = aws.String(b.KMSKeyID)
	}
	if _, err := b.Client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("uploading s3://%s/%s: %w", b.Name, key, err)
	}
	return nil
}

// Exists reports whether key exists.
func (b *Bucket) Exists(ctx context.Context, key string) (bool, error) {
	_, err := b.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.Name), Key: aws.String(key)})
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking s3://%s/%s: %w", b.Name, key, err)
}

// ObjectInfo is what Head reports about an object.
type ObjectInfo struct {
	// Metadata is the object's user metadata.
	Metadata map[string]string
	// ETag identifies this version of the object, for DeleteIfMatch.
	ETag string
}

// Head returns key's metadata and ETag, and whether key exists.
func (b *Bucket) Head(ctx context.Context, key string) (ObjectInfo, bool, error) {
	out, err := b.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.Name), Key: aws.String(key)})
	if err == nil {
		return ObjectInfo{Metadata: out.Metadata, ETag: aws.ToString(out.ETag)}, true, nil
	}
	if IsNotFound(err) {
		return ObjectInfo{}, false, nil
	}
	return ObjectInfo{}, false, fmt.Errorf("checking s3://%s/%s: %w", b.Name, key, err)
}

// DeleteIfMatch removes key only while its ETag is still etag, so an object
// replaced since it was read is kept. It reports whether key was deleted; a
// replaced or missing object is not an error.
func (b *Bucket) DeleteIfMatch(ctx context.Context, key, etag string) (bool, error) {
	_, err := b.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.Name), Key: aws.String(key), IfMatch: aws.String(etag)})
	switch {
	case err == nil:
		return true, nil
	case IsNotFound(err) || IsPreconditionFailed(err):
		return false, nil
	default:
		return false, fmt.Errorf("deleting s3://%s/%s: %w", b.Name, key, err)
	}
}

// IsPreconditionFailed reports whether err is S3 refusing a conditional
// request because the object changed.
func IsPreconditionFailed(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "PreconditionFailed"
}

// Get opens key for reading. The caller closes the body.
func (b *Bucket) Get(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	out, err := b.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.Name), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("downloading s3://%s/%s: %w", b.Name, key, err)
	}
	return out, nil
}

// Download writes key to localPath atomically: the object is written to a
// temporary file in the same directory and renamed into place, so readers
// never see a partial file. It returns the object's metadata.
func (b *Bucket) Download(ctx context.Context, key, localPath string) (map[string]string, error) {
	out, err := b.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer out.Body.Close() // nolint: errcheck
	if err := WriteFileAtomic(localPath, out.Body); err != nil {
		return nil, fmt.Errorf("writing s3://%s/%s to %s: %w", b.Name, key, localPath, err)
	}
	return out.Metadata, nil
}

// List yields every key under prefix, following pagination.
func (b *Bucket) List(ctx context.Context, prefix string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		var token *string
		for {
			opCtx, cancel := context.WithTimeout(ctx, OpTimeout)
			resp, err := b.Client.ListObjectsV2(opCtx, &s3.ListObjectsV2Input{
				Bucket:            aws.String(b.Name),
				Prefix:            aws.String(prefix),
				ContinuationToken: token,
			})
			cancel()
			if err != nil {
				yield("", fmt.Errorf("listing s3://%s/%s: %w", b.Name, prefix, err))
				return
			}
			for _, obj := range resp.Contents {
				if !yield(aws.ToString(obj.Key), nil) {
					return
				}
			}
			if !aws.ToBool(resp.IsTruncated) {
				return
			}
			token = resp.NextContinuationToken
		}
	}
}

// Delete removes key. A missing key is not an error.
func (b *Bucket) Delete(ctx context.Context, key string) error {
	if _, err := b.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.Name), Key: aws.String(key)}); err != nil && !IsNotFound(err) {
		return fmt.Errorf("deleting s3://%s/%s: %w", b.Name, key, err)
	}
	return nil
}

// DeletePrefix removes every key under prefix. It attempts all deletions and
// returns the number deleted plus every failure joined into one error.
func (b *Bucket) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	var errs []error
	deleted := 0
	for key, err := range b.List(ctx, prefix) {
		if err != nil {
			errs = append(errs, err)
			break
		}
		opCtx, cancel := context.WithTimeout(ctx, OpTimeout)
		err := b.Delete(opCtx, key)
		cancel()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}

// IsNotFound reports whether err is an S3 "no such key" error.
func IsNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	return errors.As(err, &nsk) || errors.As(err, &nf)
}

// WriteFileAtomic writes r to path through a temporary file and a rename.
func WriteFileAtomic(path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return err
	}
	return nil
}
