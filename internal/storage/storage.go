package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound                  = errors.New("storage object not found")
	ErrObjectConflict            = errors.New("storage object conflicts with expected content")
	ErrPublicEndpointUnavailable = errors.New("public object-storage endpoint is unavailable")
	ErrInvalidRange              = errors.New("invalid byte range")
)

type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
}

type Object struct {
	Body   io.ReadCloser
	Seeker io.ReadSeeker
	Info   ObjectInfo
}

type ListRequest struct {
	Prefix string
	After  string
	Limit  int
}

type ListPage struct {
	Items     []ObjectInfo
	NextAfter string
}

type Store interface {
	PutStaging(context.Context, string, io.Reader, int64) error
	Promote(context.Context, string, string, int64) error
	Open(context.Context, string, string) (Object, error)
	Stat(context.Context, string) (ObjectInfo, error)
	List(context.Context, ListRequest) (ListPage, error)
	Delete(context.Context, string) error
	Presign(context.Context, string, time.Duration) (string, error)
	Ready(context.Context) error
}
