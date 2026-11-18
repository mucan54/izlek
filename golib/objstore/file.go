package objstore

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// staleTempAge is how old an abandoned temp file must be before List removes
// it. It is deliberately long: Rust's LocalFileSystem uses the same "#<n>"
// suffix for in-progress multipart uploads, which must not be touched.
const staleTempAge = 24 * time.Hour

// FileStore is the file:// backend. An object is a regular file at
// <root>/<key>; there is no sidecar metadata. Writes go through a temp file
// named "<final>#<digits>" in the destination directory (the staging name
// convention of Rust's object_store LocalFileSystem, whose listing skips such
// names just like List here does) and become visible atomically:
// Put/PutStream by rename(2), PutIfAbsent by link(2).
type FileStore struct {
	root string
}

// NewFileStore opens (creating if needed) the store rooted at the absolute
// directory root.
func NewFileStore(root string) (*FileStore, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("objstore: file store root must be absolute, got %q", root)
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("objstore: %w", err)
	}
	return &FileStore{root: root}, nil
}

// Root returns the store's directory.
func (s *FileStore) Root() string { return s.root }

func (s *FileStore) path(key string) string {
	return filepath.Join(s.root, filepath.FromSlash(key))
}

// isTempName reports whether a file name is an in-progress upload
// ("<name>#<digits>").
func isTempName(name string) bool {
	i := strings.LastIndexByte(name, '#')
	if i < 0 || i == len(name)-1 {
		return false
	}
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func fileETag(fi os.FileInfo) string {
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}
	return fmt.Sprintf("%x-%x-%x", ino, fi.ModTime().UnixMicro(), fi.Size())
}

func fileObject(key string, fi os.FileInfo) Object {
	return Object{Key: key, Size: fi.Size(), ETag: fileETag(fi), LastModified: fi.ModTime()}
}

func (s *FileStore) Get(ctx context.Context, key string, opts *GetOptions) ([]byte, Object, error) {
	if err := checkKey(key); err != nil {
		return nil, Object{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, Object{}, err
	}
	f, err := os.Open(s.path(key))
	if err != nil {
		if isNotExist(err) {
			return nil, Object{}, fmt.Errorf("get %s: %w", key, ErrNotFound)
		}
		return nil, Object{}, fmt.Errorf("objstore: get %s: %w", key, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, Object{}, fmt.Errorf("objstore: get %s: %w", key, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, Object{}, fmt.Errorf("get %s: %w", key, ErrNotFound)
	}
	obj := fileObject(key, fi)
	if opts != nil && opts.IfNoneMatch != "" && opts.IfNoneMatch == obj.ETag {
		return nil, obj, ErrNotModified
	}
	var r io.Reader = f
	size := fi.Size()
	if opts != nil && opts.Range != nil {
		if err := checkRange(opts.Range); err != nil {
			return nil, Object{}, err
		}
		off, n := opts.Range.Offset, opts.Range.Length
		if off > size {
			off = size
		}
		if n <= 0 || off+n > size {
			n = size - off
		}
		r = io.NewSectionReader(f, off, n)
		size = n
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, Object{}, fmt.Errorf("objstore: get %s: %w", key, err)
	}
	return data, obj, nil
}

func (s *FileStore) Put(ctx context.Context, key string, data []byte) (Object, error) {
	return s.write(ctx, key, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}, false)
}

func (s *FileStore) PutIfAbsent(ctx context.Context, key string, data []byte) (Object, error) {
	return s.write(ctx, key, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}, true)
}

func (s *FileStore) PutStream(ctx context.Context, key string, r io.Reader, size int64) (Object, error) {
	return s.write(ctx, key, func(f *os.File) error {
		n, err := io.Copy(f, &ctxReader{ctx: ctx, r: r})
		if err != nil {
			return err
		}
		if size >= 0 && n != size {
			return fmt.Errorf("stream yielded %d bytes, expected %d", n, size)
		}
		return nil
	}, false)
}

// write stages the object in a temp file next to its destination, fsyncs it
// and publishes it with rename (replace) or link (create-if-absent).
func (s *FileStore) write(ctx context.Context, key string, fill func(*os.File) error, ifAbsent bool) (Object, error) {
	if err := checkKey(key); err != nil {
		return Object{}, err
	}
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	final := s.path(key)
	if isTempName(filepath.Base(final)) {
		return Object{}, fmt.Errorf("objstore: invalid key %q (name collides with temp-file pattern)", key)
	}
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
	}
	f, tmp, err := createTemp(final)
	if err != nil {
		return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
	}
	cleanup := func() { f.Close(); os.Remove(tmp) }
	if err := fill(f); err != nil {
		cleanup()
		return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		cleanup()
		return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
	}
	if ifAbsent {
		err = os.Link(tmp, final)
		os.Remove(tmp)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return Object{}, fmt.Errorf("put %s: %w", key, ErrAlreadyExists)
			}
			return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
		}
	} else if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
	}
	if err := syncDir(dir); err != nil {
		return Object{}, fmt.Errorf("objstore: put %s: %w", key, err)
	}
	return fileObject(key, fi), nil
}

func createTemp(final string) (*os.File, string, error) {
	for range 16 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, "", err
		}
		tmp := final + "#" + strconv.FormatUint(binary.LittleEndian.Uint64(b[:]), 10)
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, tmp, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not create a unique temp file")
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s *FileStore) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(s.path(key)); err != nil && !isNotExist(err) {
		return fmt.Errorf("objstore: delete %s: %w", key, err)
	}
	return nil
}

func (s *FileStore) List(ctx context.Context, prefix string) ([]Object, error) {
	// Walk from the deepest directory fully named by the prefix, then apply
	// the prefix as a plain string filter.
	dirPart := ""
	if i := strings.LastIndexByte(prefix, '/'); i >= 0 {
		dirPart = prefix[:i]
	}
	for _, seg := range strings.Split(dirPart, "/") {
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("objstore: invalid prefix %q", prefix)
		}
	}
	start := filepath.Join(s.root, filepath.FromSlash(dirPart))
	var out []Object
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if isNotExist(err) {
				return nil // raced with a delete, or the prefix does not exist
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if isTempName(d.Name()) {
			if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) > staleTempAge {
				os.Remove(p)
			}
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if isNotExist(err) {
				return nil
			}
			return err
		}
		out = append(out, fileObject(key, fi))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("objstore: list %q: %w", prefix, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func isNotExist(err error) bool {
	// ENOTDIR: a path component is a regular file, so the key cannot exist.
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// ctxReader makes a blocking copy loop observe context cancellation between
// reads.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
