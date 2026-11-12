// Package objstoretest provides an in-memory S3 fake for tests.
package objstoretest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/runatlantis/atlantis/server/core/objstore"
)

// Object is a stored object.
type Object struct {
	Body       []byte
	Metadata   map[string]string
	Encryption types.ServerSideEncryption
	KMSKeyID   string
	// ETag changes on every Put.
	ETag string
}

// Fake is an in-memory objstore.API. PageSize bounds list pages (default 2)
// so pagination is always exercised. Err, if set, is returned by every call.
type Fake struct {
	mu       sync.Mutex
	Objects  map[string]Object
	PageSize int
	Err      error
	// BeforeDelete, if set, runs at the start of every DeleteObject, so a
	// test can change the bucket between a read and a delete.
	BeforeDelete func(key string)
	puts         int
}

var _ objstore.API = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake { return &Fake{Objects: map[string]Object{}} }

func (f *Fake) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, f.Err
}

func (f *Fake) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	o, ok := f.Objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NotFound{}
	}
	return &s3.HeadObjectOutput{Metadata: o.Metadata, ETag: aws.String(o.ETag), ContentLength: aws.Int64(int64(len(o.Body)))}, nil
}

func (f *Fake) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	f.Objects[aws.ToString(in.Key)] = Object{Body: body, Metadata: in.Metadata, Encryption: in.ServerSideEncryption, KMSKeyID: aws.ToString(in.SSEKMSKeyId), ETag: fmt.Sprintf("%q", fmt.Sprint("etag-", f.puts))}
	return &s3.PutObjectOutput{}, nil
}

func (f *Fake) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	o, ok := f.Objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(o.Body)), Metadata: o.Metadata}, nil
}

func (f *Fake) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if f.BeforeDelete != nil {
		f.BeforeDelete(aws.ToString(in.Key))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	// Like S3, a conditional delete fails on a missing or changed object.
	o, ok := f.Objects[aws.ToString(in.Key)]
	if in.IfMatch != nil && !ok {
		return nil, &types.NoSuchKey{}
	}
	if ok && in.IfMatch != nil && aws.ToString(in.IfMatch) != o.ETag {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold"}
	}
	delete(f.Objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (f *Fake) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var keys []string
	for k := range f.Objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	// Like S3, a continuation token resumes after the last key returned, so
	// deleting keys while paginating does not skip or repeat any.
	start := 0
	if in.ContinuationToken != nil {
		start, _ = slices.BinarySearch(keys, *in.ContinuationToken)
		if start < len(keys) && keys[start] == *in.ContinuationToken {
			start++
		}
	}
	size := f.PageSize
	if size == 0 {
		size = 2
	}
	end := min(start+size, len(keys))
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(end < len(keys))}
	for _, k := range keys[start:end] {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k)})
	}
	if end < len(keys) {
		out.NextContinuationToken = aws.String(keys[end-1])
	}
	return out, nil
}

// Keys returns the stored keys, sorted.
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.Objects))
	for k := range f.Objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
