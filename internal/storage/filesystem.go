package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const readinessFilename = ".readiness"

type Filesystem struct {
	root string
}

var _ Store = (*Filesystem)(nil)

func NewFilesystem(root string) (*Filesystem, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("filesystem storage root must be an absolute path")
	}
	root = filepath.Clean(root)
	if root == filepath.VolumeName(root)+string(os.PathSeparator) {
		return nil, fmt.Errorf("filesystem storage root must not be a filesystem root")
	}
	if err := os.MkdirAll(root, 0750); err != nil {
		return nil, fmt.Errorf("create filesystem storage root %q: %w", root, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve filesystem storage root %q: %w", root, err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return nil, fmt.Errorf("stat filesystem storage root %q: %w", resolvedRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("filesystem storage root %q is not a directory", resolvedRoot)
	}
	store := &Filesystem{root: resolvedRoot}
	if err := store.Ready(context.Background()); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Filesystem) PutStaging(ctx context.Context, key string, reader io.Reader, size int64) error {
	if reader == nil || size < 0 {
		return fmt.Errorf("put staging object: invalid request")
	}
	objectPath, err := s.objectPath(key)
	if err != nil {
		return fmt.Errorf("put staging object: %w", err)
	}
	parent, err := s.ensureParent(key)
	if err != nil {
		return fmt.Errorf("prepare staging object %q: %w", key, err)
	}
	file, err := os.OpenFile(objectPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("put staging object %q: %w", key, ErrObjectConflict)
	}
	if err != nil {
		return fmt.Errorf("create staging object %q: %w", key, err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(objectPath)
		}
	}()

	written, err := io.Copy(file, contextReader{ctx: ctx, reader: reader})
	if err != nil {
		return fmt.Errorf("write staging object %q: %w", key, err)
	}
	if written != size {
		return fmt.Errorf("write staging object %q: size %d does not match expected %d", key, written, size)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync staging object %q: %w", key, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close staging object %q: %w", key, err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync staging directory for %q: %w", key, err)
	}
	keep = true
	return nil
}

func (s *Filesystem) Promote(ctx context.Context, stagingKey, objectKey string, expectedSize int64) error {
	if expectedSize < 0 || stagingKey == objectKey {
		return fmt.Errorf("promote staging object: invalid request")
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	stagingPath, err := s.objectPath(stagingKey)
	if err != nil {
		return fmt.Errorf("promote staging object: %w", err)
	}
	objectPath, err := s.objectPath(objectKey)
	if err != nil {
		return fmt.Errorf("promote staging object: %w", err)
	}
	stagingInfo, err := s.regularInfo(stagingKey, stagingPath)
	if err != nil {
		return fmt.Errorf("stat staging object %q: %w", stagingKey, err)
	}
	if stagingInfo.Size() != expectedSize {
		return fmt.Errorf("promote staging object %q: %w", stagingKey, ErrObjectConflict)
	}

	if objectInfo, statErr := s.regularInfo(objectKey, objectPath); statErr == nil {
		if objectInfo.Size() != expectedSize {
			return fmt.Errorf("promote staging object %q to %q: %w", stagingKey, objectKey, ErrObjectConflict)
		}
		if err := os.Chmod(objectPath, 0440); err != nil {
			return fmt.Errorf("make promoted object %q read-only: %w", objectKey, err)
		}
		return s.removePromotedStaging(stagingKey, stagingPath)
	} else if !errors.Is(statErr, ErrNotFound) {
		return fmt.Errorf("stat promoted object %q: %w", objectKey, statErr)
	}

	destinationParent, err := s.ensureParent(objectKey)
	if err != nil {
		return fmt.Errorf("prepare promoted object %q: %w", objectKey, err)
	}
	// Set the immutable mode before publishing the hard link. Once the link is
	// visible, it is intentionally left in place even if directory fsync fails;
	// a retry can then safely deduplicate it without losing the only durable
	// copy during a concurrent promotion.
	if err := os.Chmod(stagingPath, 0440); err != nil {
		return fmt.Errorf("make staging object %q read-only before promotion: %w", stagingKey, err)
	}
	if err := os.Link(stagingPath, objectPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			objectInfo, statErr := s.regularInfo(objectKey, objectPath)
			if statErr != nil || objectInfo.Size() != expectedSize {
				return fmt.Errorf("promote staging object %q to %q: %w", stagingKey, objectKey, ErrObjectConflict)
			}
			if err := os.Chmod(objectPath, 0440); err != nil {
				return fmt.Errorf("make promoted object %q read-only: %w", objectKey, err)
			}
			if err := syncDirectory(destinationParent); err != nil {
				return fmt.Errorf("sync promoted object directory for %q: %w", objectKey, err)
			}
			return s.removePromotedStaging(stagingKey, stagingPath)
		}
		return fmt.Errorf("atomically link staging object %q to %q: %w", stagingKey, objectKey, err)
	}
	if err := syncDirectory(destinationParent); err != nil {
		return fmt.Errorf("sync promoted object directory for %q: %w", objectKey, err)
	}
	return s.removePromotedStaging(stagingKey, stagingPath)
}

func (s *Filesystem) Open(ctx context.Context, key, rangeHeader string) (Object, error) {
	if err := contextError(ctx); err != nil {
		return Object{}, err
	}
	if rangeHeader != "" && (!strings.HasPrefix(rangeHeader, "bytes=") || strings.ContainsAny(rangeHeader, "\r\n")) {
		return Object{}, ErrInvalidRange
	}
	objectPath, err := s.objectPath(key)
	if err != nil {
		return Object{}, fmt.Errorf("open object: %w", err)
	}
	if err := s.checkParents(key); err != nil {
		return Object{}, fmt.Errorf("open object %q: %w", key, err)
	}
	file, err := os.Open(objectPath)
	if errors.Is(err, os.ErrNotExist) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, fmt.Errorf("open object %q: %w", key, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return Object{}, fmt.Errorf("stat opened object %q: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return Object{}, fmt.Errorf("open object %q: object is not a regular file", key)
	}
	return Object{Body: file, Seeker: file, Info: filesystemObjectInfo(key, info)}, nil
}

func (s *Filesystem) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := contextError(ctx); err != nil {
		return ObjectInfo{}, err
	}
	objectPath, err := s.objectPath(key)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("stat object: %w", err)
	}
	info, err := s.regularInfo(key, objectPath)
	if err != nil {
		return ObjectInfo{}, err
	}
	return filesystemObjectInfo(key, info), nil
}

func (s *Filesystem) List(ctx context.Context, request ListRequest) (ListPage, error) {
	if request.Prefix == "" || request.Limit < 1 || request.Limit > 1000 {
		return ListPage{}, fmt.Errorf("list objects: prefix and limit between 1 and 1000 are required")
	}
	if _, err := validateObjectKey(strings.TrimSuffix(request.Prefix, "/")); err != nil {
		return ListPage{}, fmt.Errorf("list objects: invalid prefix: %w", err)
	}
	if request.After != "" && !strings.HasPrefix(request.After, request.Prefix) {
		return ListPage{}, fmt.Errorf("list objects: cursor is outside prefix")
	}

	items := make([]ObjectInfo, 0)
	err := filepath.WalkDir(s.root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		if filePath == s.root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("filesystem storage contains forbidden symbolic link %q", filePath)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("filesystem storage contains non-regular object %q", filePath)
		}
		relative, err := filepath.Rel(s.root, filePath)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if !strings.HasPrefix(key, request.Prefix) || key <= request.After {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		items = append(items, filesystemObjectInfo(key, info))
		return nil
	})
	if err != nil {
		return ListPage{}, fmt.Errorf("list filesystem objects with prefix %q: %w", request.Prefix, err)
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Key < items[right].Key })
	page := ListPage{Items: items}
	if len(page.Items) > request.Limit {
		page.Items = page.Items[:request.Limit]
	}
	if len(page.Items) == request.Limit {
		page.NextAfter = page.Items[len(page.Items)-1].Key
	}
	return page, nil
}

func (s *Filesystem) Delete(ctx context.Context, key string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	objectPath, err := s.objectPath(key)
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	if _, err := s.regularInfo(key, objectPath); errors.Is(err, ErrNotFound) {
		return nil
	} else if err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	if err := os.Remove(objectPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	if err := syncDirectory(filepath.Dir(objectPath)); err != nil {
		return fmt.Errorf("sync directory after deleting object %q: %w", key, err)
	}
	return nil
}

func (s *Filesystem) Presign(context.Context, string, time.Duration) (string, error) {
	return "", ErrPublicEndpointUnavailable
}

func (s *Filesystem) Ready(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	info, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("probe filesystem storage root %q: %w", s.root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("probe filesystem storage root %q: not a directory", s.root)
	}
	probePath := filepath.Join(s.root, readinessFilename)
	if probeInfo, statErr := os.Lstat(probePath); statErr == nil && probeInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("probe filesystem storage root %q: readiness file is a symbolic link", s.root)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("probe filesystem storage root %q: %w", s.root, statErr)
	}
	probe, err := os.OpenFile(probePath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("probe filesystem storage root %q for writes: %w", s.root, err)
	}
	if err := probe.Close(); err != nil {
		return fmt.Errorf("close filesystem storage readiness file: %w", err)
	}
	return nil
}

func (s *Filesystem) objectPath(key string) (string, error) {
	cleanKey, err := validateObjectKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(cleanKey)), nil
}

func validateObjectKey(key string) (string, error) {
	if key == "" || strings.ContainsRune(key, '\x00') || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("invalid object key")
	}
	clean := path.Clean(key)
	if clean == "." || clean != key || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid object key")
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("invalid object key")
		}
	}
	return clean, nil
}

func (s *Filesystem) ensureParent(key string) (string, error) {
	cleanKey, err := validateObjectKey(key)
	if err != nil {
		return "", err
	}
	segments := strings.Split(path.Dir(cleanKey), "/")
	current := s.root
	if len(segments) == 1 && segments[0] == "." {
		return current, nil
	}
	for _, segment := range segments {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0750); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("storage path component %q is not a directory", current)
		}
	}
	return current, nil
}

func (s *Filesystem) checkParents(key string) error {
	cleanKey, err := validateObjectKey(key)
	if err != nil {
		return err
	}
	current := s.root
	segments := strings.Split(path.Dir(cleanKey), "/")
	if len(segments) == 1 && segments[0] == "." {
		return nil
	}
	for _, segment := range segments {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("storage path component %q is not a directory", current)
		}
	}
	return nil
}

func (s *Filesystem) regularInfo(key, objectPath string) (os.FileInfo, error) {
	if err := s.checkParents(key); err != nil {
		return nil, err
	}
	info, err := os.Lstat(objectPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("object is not a regular file")
	}
	return info, nil
}

func (s *Filesystem) removePromotedStaging(key, objectPath string) error {
	if err := os.Remove(objectPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete promoted staging object %q: %w", key, err)
	}
	if err := syncDirectory(filepath.Dir(objectPath)); err != nil {
		return fmt.Errorf("sync staging directory after promoting %q: %w", key, err)
	}
	return nil
}

func filesystemObjectInfo(key string, info os.FileInfo) ObjectInfo {
	return ObjectInfo{
		Key:          key,
		Size:         info.Size(),
		ETag:         fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano()),
		ContentType:  "application/octet-stream",
		LastModified: info.ModTime().UTC(),
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("storage context is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := contextError(r.ctx); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	} else {
		return err
	}
}
