package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinIOOptions struct {
	Endpoint       string
	PublicEndpoint string
	AccessKey      string
	SecretKey      string
	Bucket         string
	Region         string
	UseTLS         bool
}

type MinIO struct {
	internal *minio.Client
	public   *minio.Client
	bucket   string
}

func NewMinIO(options MinIOOptions) (*MinIO, error) {
	if options.Endpoint == "" || options.AccessKey == "" || options.SecretKey == "" || options.Bucket == "" {
		return nil, fmt.Errorf("MinIO endpoint, credentials, and bucket are required")
	}
	internalEndpoint, internalTLS, err := normalizeEndpoint(options.Endpoint, options.UseTLS)
	if err != nil {
		return nil, fmt.Errorf("invalid internal MinIO endpoint: %w", err)
	}
	credentialsProvider := credentials.NewStaticV4(options.AccessKey, options.SecretKey, "")
	internalClient, err := minio.New(internalEndpoint, &minio.Options{
		Creds:  credentialsProvider,
		Secure: internalTLS,
		Region: options.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create internal MinIO client: %w", err)
	}

	var publicClient *minio.Client
	if options.PublicEndpoint != "" {
		publicEndpoint, publicTLS, err := normalizeEndpoint(options.PublicEndpoint, options.UseTLS)
		if err != nil {
			return nil, fmt.Errorf("invalid public MinIO endpoint: %w", err)
		}
		publicClient, err = minio.New(publicEndpoint, &minio.Options{
			Creds:  credentialsProvider,
			Secure: publicTLS,
			Region: options.Region,
		})
		if err != nil {
			return nil, fmt.Errorf("create public MinIO client: %w", err)
		}
	}
	return &MinIO{internal: internalClient, public: publicClient, bucket: options.Bucket}, nil
}

func (s *MinIO) PutStaging(ctx context.Context, key string, reader io.Reader, size int64) error {
	if key == "" || reader == nil || size < 0 {
		return fmt.Errorf("put staging object: invalid request")
	}
	if _, err := s.internal.PutObject(ctx, s.bucket, key, reader, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	}); err != nil {
		return fmt.Errorf("put staging object %q: %w", key, err)
	}
	return nil
}

func (s *MinIO) Promote(ctx context.Context, stagingKey, objectKey string, expectedSize int64) error {
	if stagingKey == "" || objectKey == "" || expectedSize < 0 {
		return fmt.Errorf("promote staging object: invalid request")
	}
	info, err := s.internal.StatObject(ctx, s.bucket, objectKey, minio.StatObjectOptions{})
	if err == nil {
		if info.Size != expectedSize {
			return ErrObjectConflict
		}
		if err := s.internal.RemoveObject(ctx, s.bucket, stagingKey, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("delete deduplicated staging object %q: %w", stagingKey, err)
		}
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("stat promoted object %q: %w", objectKey, err)
	}

	if _, err := s.internal.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucket, Object: objectKey},
		minio.CopySrcOptions{Bucket: s.bucket, Object: stagingKey},
	); err != nil {
		return fmt.Errorf("copy staging object %q to %q: %w", stagingKey, objectKey, err)
	}
	promoted, err := s.internal.StatObject(ctx, s.bucket, objectKey, minio.StatObjectOptions{})
	if err != nil {
		return fmt.Errorf("stat copied object %q: %w", objectKey, err)
	}
	if promoted.Size != expectedSize {
		return ErrObjectConflict
	}
	if err := s.internal.RemoveObject(ctx, s.bucket, stagingKey, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete promoted staging object %q: %w", stagingKey, err)
	}
	return nil
}

func (s *MinIO) Open(ctx context.Context, key, rangeHeader string) (Object, error) {
	options := minio.GetObjectOptions{}
	if rangeHeader != "" {
		if !strings.HasPrefix(rangeHeader, "bytes=") || strings.ContainsAny(rangeHeader, "\r\n") {
			return Object{}, ErrInvalidRange
		}
		options.Set("Range", rangeHeader)
	}
	object, err := s.internal.GetObject(ctx, s.bucket, key, options)
	if err != nil {
		return Object{}, mapObjectError("open", key, err)
	}
	info, err := object.Stat()
	if err != nil {
		_ = object.Close()
		return Object{}, mapObjectError("stat opened", key, err)
	}
	return Object{Body: object, Seeker: object, Info: objectInfo(info)}, nil
}

func (s *MinIO) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := s.internal.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, mapObjectError("stat", key, err)
	}
	return objectInfo(info), nil
}

func (s *MinIO) List(ctx context.Context, request ListRequest) (ListPage, error) {
	if request.Prefix == "" || request.Limit < 1 || request.Limit > 1000 {
		return ListPage{}, fmt.Errorf("list objects: prefix and limit between 1 and 1000 are required")
	}
	if request.After != "" && !strings.HasPrefix(request.After, request.Prefix) {
		return ListPage{}, fmt.Errorf("list objects: cursor is outside prefix")
	}

	listContext, cancel := context.WithCancel(ctx)
	defer cancel()
	page := ListPage{Items: make([]ObjectInfo, 0, request.Limit)}
	objects := s.internal.ListObjects(listContext, s.bucket, minio.ListObjectsOptions{
		Prefix:     request.Prefix,
		Recursive:  true,
		MaxKeys:    request.Limit,
		StartAfter: request.After,
	})
	pageFull := false
	for object := range objects {
		if object.Err != nil {
			if pageFull && errors.Is(object.Err, context.Canceled) {
				continue
			}
			return ListPage{}, fmt.Errorf("list objects with prefix %q: %w", request.Prefix, object.Err)
		}
		if pageFull {
			continue
		}
		page.Items = append(page.Items, objectInfo(object))
		if len(page.Items) == request.Limit {
			page.NextAfter = object.Key
			pageFull = true
			cancel()
		}
	}
	return page, nil
}

func (s *MinIO) Delete(ctx context.Context, key string) error {
	if err := s.internal.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

func (s *MinIO) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s.public == nil {
		return "", ErrPublicEndpointUnavailable
	}
	if key == "" || ttl <= 0 {
		return "", fmt.Errorf("presign object: invalid request")
	}
	signed, err := s.public.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("presign object %q: %w", key, err)
	}
	return signed.String(), nil
}

func (s *MinIO) Ready(ctx context.Context) error {
	exists, err := s.internal.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("probe MinIO bucket %q: %w", s.bucket, err)
	}
	if !exists {
		return fmt.Errorf("MinIO bucket %q does not exist", s.bucket)
	}
	return nil
}

func normalizeEndpoint(value string, fallbackTLS bool) (string, bool, error) {
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", false, fmt.Errorf("endpoint must be an HTTP(S) origin")
		}
		switch parsed.Scheme {
		case "http":
			return parsed.Host, false, nil
		case "https":
			return parsed.Host, true, nil
		default:
			return "", false, fmt.Errorf("endpoint scheme must be http or https")
		}
	}
	if strings.ContainsAny(value, "/?#") {
		return "", false, fmt.Errorf("endpoint must contain only host and optional port")
	}
	return value, fallbackTLS, nil
}

func isNotFound(err error) bool {
	response := minio.ToErrorResponse(err)
	return response.StatusCode == http.StatusNotFound || response.Code == "NoSuchKey" || response.Code == "NoSuchObject"
}

func mapObjectError(operation, key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%s object %q: %w", operation, key, ErrNotFound)
	}
	return fmt.Errorf("%s object %q: %w", operation, key, err)
}

func objectInfo(info minio.ObjectInfo) ObjectInfo {
	return ObjectInfo{
		Key:          info.Key,
		Size:         info.Size,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		LastModified: info.LastModified.UTC(),
	}
}
