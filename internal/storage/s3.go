package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/crypto"
)

// S3Client wraps the AWS SDK v2 S3 client for any S3-compatible object storage.
type S3Client struct {
	s3       *s3.Client
	uploader *manager.Uploader
	bucket   string
	encKey   []byte // nil = encryption disabled
}

// NewS3Client builds a standard S3 client from the provided config.
// The BaseEndpoint field makes it compatible with any S3-compatible provider.
func NewS3Client(cfg config.S3Config, encCfg config.EncryptionConfig) (*S3Client, error) {
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("s3: access_key and secret_key are required")
	}

	encKey, err := crypto.ParseKey(encCfg.Key)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}

	s3Client := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		Credentials: aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		),
		UsePathStyle: true,
	})

	return &S3Client{
		s3:       s3Client,
		uploader: manager.NewUploader(s3Client),
		bucket:   cfg.Bucket,
		encKey:   encKey,
	}, nil
}

// Upload stores a file stream as objectKey in the configured bucket.
// If encryption is enabled the stream is encrypted before upload.
// Uses multipart upload to support non-seekable streams (io.Pipe).
func (c *S3Client) Upload(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error {
	body := crypto.Encrypt(c.encKey, reader)
	defer body.Close()

	_, err := c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(objectKey),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("uploading %q to S3 bucket %q: %w", objectKey, c.bucket, err)
	}
	return nil
}

// GetObject downloads an object and returns its content as bytes.
// If encryption is enabled the data is decrypted before being returned.
func (c *S3Client) GetObject(ctx context.Context, objectKey string) ([]byte, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, fmt.Errorf("getting S3 object %q: %w", objectKey, err)
	}

	body := crypto.Decrypt(c.encKey, out.Body)
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading S3 object %q: %w", objectKey, err)
	}
	return data, nil
}

// DeleteObject removes an object from the configured bucket.
func (c *S3Client) DeleteObject(ctx context.Context, objectKey string) error {
	if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	}); err != nil {
		return fmt.Errorf("deleting S3 object %q: %w", objectKey, err)
	}
	return nil
}
