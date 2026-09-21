package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func (clients *awsClients) encryptAndUploadBundle(ctx context.Context, bucket, key, pushTip, filename string, recipients [][]byte) error {
	// A retry should not transfer a completed multipart object again merely
	// to discover the conditional-write conflict at completion.
	existing, err := clients.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err == nil {
		if existing.Metadata[bundlePushTipMetadata] != pushTip || aws.ToInt64(existing.ContentLength) <= 0 {
			return fmt.Errorf("refusing existing bundle with different or unknown push tip: %s", key)
		}
		fmt.Fprintln(os.Stderr, "reuse completed bundle from the same push:", key)
		return nil
	}
	var missing *s3types.NotFound
	if !errors.As(err, &missing) {
		return fmt.Errorf("inspect upload destination %s: %w", key, err)
	}
	plain, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() { _ = plain.Close() }()
	encrypted := filename + ".encrypted"
	defer func() { _ = os.Remove(encrypted) }()
	cipher, err := os.Create(encrypted)
	if err != nil {
		return err
	}
	defer func() { _ = cipher.Close() }()
	if err := encryptPushBundle(ctx, recipients, plain, cipher); err != nil {
		return err
	}
	if err := errors.Join(plain.Close(), cipher.Close()); err != nil {
		return err
	}
	file, err := os.Open(encrypted)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	fmt.Fprintln(os.Stderr, "put s3://"+bucket+"/"+key)
	return clients.uploadBundle(ctx, bucket, key, pushTip, file)
}

const (
	bundleUploadPartSize  int64 = 64 << 20
	maxUploadPartSize     int64 = 5 << 30
	maxUploadParts              = 10000
	bundlePushTipMetadata       = "git-remote-aws-push-tip"
)

func (clients *awsClients) uploadBundle(ctx context.Context, bucket, key, pushTip string, file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > bundleUploadPartSize {
		return clients.multipartUploadBundle(ctx, bucket, key, pushTip, file, info.Size())
	}
	_, err = clients.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: file,
		IfNoneMatch: aws.String("*"), Metadata: map[string]string{bundlePushTipMetadata: pushTip},
	})
	return clients.bundleUploadResult(ctx, bucket, key, pushTip, err)
}

func multipartBundlePartSize(size int64) (int64, error) {
	// Grow parts for exceptionally large indivisible increments, while keeping
	// every retry seekable and bounded and staying within S3's 10,000-part limit.
	partSize := max(bundleUploadPartSize, size/maxUploadParts)
	if size/partSize >= maxUploadParts && size%partSize != 0 {
		partSize++
	}
	if partSize > maxUploadPartSize {
		return 0, fmt.Errorf("encrypted bundle is too large for S3 multipart upload: %d bytes", size)
	}
	return partSize, nil
}

func (clients *awsClients) multipartUploadBundle(ctx context.Context, bucket, key, pushTip string, file *os.File, size int64) (err error) {
	partSize, err := multipartBundlePartSize(size)
	if err != nil {
		return err
	}
	created, err := clients.s3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Metadata:          map[string]string{bundlePushTipMetadata: pushTip},
		ChecksumAlgorithm: s3types.ChecksumAlgorithmCrc32, ChecksumType: s3types.ChecksumTypeComposite,
	})
	if err != nil {
		return fmt.Errorf("start multipart bundle upload (an uncertain response may leave an incomplete upload): %w", err)
	}
	if aws.ToString(created.UploadId) == "" {
		return fmt.Errorf("S3 returned no multipart upload ID; inspect incomplete uploads for s3://%s/%s", bucket, key)
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		// Lease loss and helper cancellation must still release uploaded parts.
		// Abort names only this upload ID; never delete a completed object, even
		// when CompleteMultipartUpload's response was lost.
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, abortErr := clients.s3.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
		})
		var absent *s3types.NoSuchUpload
		if abortErr != nil && !errors.As(abortErr, &absent) {
			err = errors.Join(err, fmt.Errorf("abort multipart upload %s for s3://%s/%s: %w", aws.ToString(created.UploadId), bucket, key, abortErr))
		}
	}()
	var parts []s3types.CompletedPart
	for offset := int64(0); offset < size; offset += partSize {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		number := int32(len(parts) + 1)
		length := min(partSize, size-offset)
		fmt.Fprintf(os.Stderr, "multipart: part %d (%d bytes)\n", number, length)
		part, err := clients.s3.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
			PartNumber: aws.Int32(number), ContentLength: aws.Int64(length),
			Body: io.NewSectionReader(file, offset, length), ChecksumAlgorithm: s3types.ChecksumAlgorithmCrc32,
		})
		if err != nil {
			return fmt.Errorf("upload bundle part %d: %w", number, err)
		}
		if aws.ToString(part.ETag) == "" || aws.ToString(part.ChecksumCRC32) == "" {
			return fmt.Errorf("S3 returned no ETag or CRC32 checksum for bundle part %d", number)
		}
		parts = append(parts, s3types.CompletedPart{PartNumber: aws.Int32(number), ETag: part.ETag, ChecksumCRC32: part.ChecksumCRC32})
	}
	out, err := clients.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
		IfNoneMatch:     aws.String("*"), ChecksumType: s3types.ChecksumTypeComposite,
	})
	if err != nil {
		return clients.bundleUploadResult(ctx, bucket, key, pushTip, err)
	}
	if aws.ToString(out.ETag) == "" {
		return fmt.Errorf("multipart completion returned no ETag; upload outcome unknown for s3://%s/%s", bucket, key)
	}
	completed = true
	return nil
}

func (clients *awsClients) bundleUploadResult(ctx context.Context, bucket, key, pushTip string, uploadErr error) error {
	var apiErr smithy.APIError
	if !errors.As(uploadErr, &apiErr) || apiErr.ErrorCode() != "PreconditionFailed" {
		return uploadErr
	}
	// Different pushes can share intermediate range names but encrypt for
	// different recipients. Never overwrite ciphertext or adopt another push's
	// orphan. A matching immutable tip pins the same committed recipient policy
	// and permits safe retries after a partial upload or uncertain publication.
	out, err := clients.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return errors.Join(uploadErr, fmt.Errorf("inspect existing bundle: %w", err))
	}
	if out.Metadata[bundlePushTipMetadata] != pushTip || aws.ToInt64(out.ContentLength) <= 0 {
		return fmt.Errorf("refusing to overwrite existing bundle s3://%s/%s with a different or unknown push tip; inspect incomplete pushes: %w", bucket, key, uploadErr)
	}
	fmt.Fprintln(os.Stderr, "reuse completed bundle from the same push:", key)
	return nil
}
