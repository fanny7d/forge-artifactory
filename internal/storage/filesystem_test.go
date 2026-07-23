package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFilesystemStoreLifecycle(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystem(root)
	if err != nil {
		t.Fatalf("NewFilesystem() error = %v", err)
	}
	content := []byte("immutable artifact bytes")
	stagingKey := "staging/uploads/11111111-1111-4111-8111-111111111111"
	objectKey := "blobs/sha256/aa/bb/aabbcc"

	if err := store.PutStaging(t.Context(), stagingKey, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("PutStaging() error = %v", err)
	}
	stagingInfo, err := store.Stat(t.Context(), stagingKey)
	if err != nil {
		t.Fatalf("Stat(staging) error = %v", err)
	}
	if stagingInfo.Key != stagingKey || stagingInfo.Size != int64(len(content)) || stagingInfo.LastModified.IsZero() {
		t.Fatalf("staging info = %#v", stagingInfo)
	}

	if err := store.Promote(t.Context(), stagingKey, objectKey, int64(len(content))); err != nil {
		t.Fatalf("Promote() error = %v", err)
	}
	if _, err := store.Stat(t.Context(), stagingKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat(staging) error = %v, want ErrNotFound", err)
	}
	objectInfo, err := store.Stat(t.Context(), objectKey)
	if err != nil {
		t.Fatalf("Stat(object) error = %v", err)
	}
	if objectInfo.Size != int64(len(content)) {
		t.Fatalf("object size = %d, want %d", objectInfo.Size, len(content))
	}
	mode, err := os.Stat(filepath.Join(root, filepath.FromSlash(objectKey)))
	if err != nil {
		t.Fatalf("stat promoted file: %v", err)
	}
	if mode.Mode().Perm()&0222 != 0 {
		t.Fatalf("promoted mode = %o, want no write bits", mode.Mode().Perm())
	}

	opened, err := store.Open(t.Context(), objectKey, "")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = opened.Body.Close() }()
	read, err := io.ReadAll(opened.Body)
	if err != nil {
		t.Fatalf("read opened object: %v", err)
	}
	if !bytes.Equal(read, content) {
		t.Fatalf("opened content = %q, want %q", read, content)
	}
	if _, err := store.Presign(t.Context(), objectKey, time.Minute); !errors.Is(err, ErrPublicEndpointUnavailable) {
		t.Fatalf("Presign() error = %v, want ErrPublicEndpointUnavailable", err)
	}

	if err := store.Delete(t.Context(), objectKey); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(t.Context(), objectKey); err != nil {
		t.Fatalf("second Delete() error = %v", err)
	}
}

func TestFilesystemListIsStableAndPaged(t *testing.T) {
	store, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() error = %v", err)
	}
	for _, key := range []string{"staging/uploads/c", "staging/uploads/a", "staging/uploads/b", "blobs/sha256/a"} {
		if err := store.PutStaging(t.Context(), key, bytes.NewReader([]byte(key)), int64(len(key))); err != nil {
			t.Fatalf("PutStaging(%q) error = %v", key, err)
		}
	}

	first, err := store.List(t.Context(), ListRequest{Prefix: "staging/", Limit: 2})
	if err != nil {
		t.Fatalf("first List() error = %v", err)
	}
	if got := objectKeys(first.Items); !reflect.DeepEqual(got, []string{"staging/uploads/a", "staging/uploads/b"}) {
		t.Fatalf("first keys = %#v", got)
	}
	if first.NextAfter != "staging/uploads/b" {
		t.Fatalf("first NextAfter = %q", first.NextAfter)
	}
	second, err := store.List(t.Context(), ListRequest{Prefix: "staging/", After: first.NextAfter, Limit: 2})
	if err != nil {
		t.Fatalf("second List() error = %v", err)
	}
	if got := objectKeys(second.Items); !reflect.DeepEqual(got, []string{"staging/uploads/c"}) {
		t.Fatalf("second keys = %#v", got)
	}
	if second.NextAfter != "" {
		t.Fatalf("second NextAfter = %q, want empty", second.NextAfter)
	}
}

func TestFilesystemPutCleansIncompleteObjects(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystem(root)
	if err != nil {
		t.Fatalf("NewFilesystem() error = %v", err)
	}
	key := "staging/uploads/incomplete"
	if err := store.PutStaging(t.Context(), key, bytes.NewReader([]byte("short")), 20); err == nil {
		t.Fatal("PutStaging() error = nil, want size mismatch")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(key))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete object stat error = %v, want not exist", err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.PutStaging(canceled, key, bytes.NewReader([]byte("payload")), 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled PutStaging() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(key))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled object stat error = %v, want not exist", err)
	}
}

func TestFilesystemPromoteHandlesDeduplicationAndConflict(t *testing.T) {
	store, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() error = %v", err)
	}
	objectKey := "blobs/sha256/aa/bb/value"
	if err := store.PutStaging(t.Context(), "staging/first", bytes.NewReader([]byte("first")), 5); err != nil {
		t.Fatalf("PutStaging(first) error = %v", err)
	}
	if err := store.Promote(t.Context(), "staging/first", objectKey, 5); err != nil {
		t.Fatalf("Promote(first) error = %v", err)
	}
	if err := store.PutStaging(t.Context(), "staging/deduplicated", bytes.NewReader([]byte("other")), 5); err != nil {
		t.Fatalf("PutStaging(deduplicated) error = %v", err)
	}
	if err := store.Promote(t.Context(), "staging/deduplicated", objectKey, 5); err != nil {
		t.Fatalf("Promote(deduplicated) error = %v", err)
	}
	if _, err := store.Stat(t.Context(), "staging/deduplicated"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deduplicated staging error = %v, want ErrNotFound", err)
	}
	if err := store.PutStaging(t.Context(), "staging/conflict", bytes.NewReader([]byte("different")), 9); err != nil {
		t.Fatalf("PutStaging(conflict) error = %v", err)
	}
	if err := store.Promote(t.Context(), "staging/conflict", objectKey, 9); !errors.Is(err, ErrObjectConflict) {
		t.Fatalf("Promote(conflict) error = %v, want ErrObjectConflict", err)
	}
	if _, err := store.Stat(t.Context(), "staging/conflict"); err != nil {
		t.Fatalf("conflicting staging object was removed: %v", err)
	}
	if err := store.Promote(t.Context(), "staging/conflict", "staging/conflict", 9); err == nil {
		t.Fatal("Promote(same key) error = nil")
	}
}

func TestFilesystemRejectsUnsafePathsAndSymlinks(t *testing.T) {
	if _, err := NewFilesystem("relative/path"); err == nil {
		t.Fatal("NewFilesystem(relative) error = nil")
	}
	if _, err := NewFilesystem(string(os.PathSeparator)); err == nil {
		t.Fatal("NewFilesystem(root) error = nil")
	}
	root := t.TempDir()
	store, err := NewFilesystem(root)
	if err != nil {
		t.Fatalf("NewFilesystem() error = %v", err)
	}
	for _, key := range []string{"", "/absolute", "../escape", "staging/../escape", "staging//escape", `staging\\escape`} {
		if err := store.PutStaging(t.Context(), key, bytes.NewReader(nil), 0); err == nil {
			t.Fatalf("PutStaging(%q) error = nil", key)
		}
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if err := store.PutStaging(t.Context(), "linked/object", bytes.NewReader([]byte("payload")), 7); err == nil {
		t.Fatal("PutStaging through symlink error = nil")
	}
	if _, err := os.Stat(filepath.Join(outside, "object")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside object stat error = %v, want not exist", err)
	}
}

func objectKeys(items []ObjectInfo) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	return keys
}
