package fsh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/afero"
)

const DefaultDirPerm = 0o755

var _ FS = (*RealFs)(nil)

type RealFs struct {
	fs *afero.OsFs
}

func NewRealFS() FS {
	return &RealFs{
		fs: afero.NewOsFs().(*afero.OsFs),
	}
}

func (r *RealFs) MkdirAll(path string, perm os.FileMode) error {
	return r.fs.MkdirAll(path, perm)
}

func (r *RealFs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	return r.fs.OpenFile(name, flag, perm)
}

func (r *RealFs) Remove(name string) error {
	return r.fs.Remove(name)
}

func (r *RealFs) RemoveAll(path string) error {
	return r.fs.RemoveAll(path)
}

func (r *RealFs) Stat(name string) (os.FileInfo, error) {
	return r.fs.Stat(name)
}

func (r *RealFs) SymlinkIfPossible(oldname, newname string) error {
	return r.fs.SymlinkIfPossible(oldname, newname)
}

func (r *RealFs) GetCurrentDir() string {
	dir, err := os.Getwd()
	if err != nil {
		// FIXME(zhuravlev): fix
		panic("current ir is not defined: " + err.Error())
	}

	return dir
}

func (r *RealFs) GetHomeDir() (string, error) {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}

	return dir, nil
}

func (r *RealFs) Walk(root string, fn filepath.WalkFunc) error {
	return afero.Walk(r.fs, root, fn)
}

func (r *RealFs) Name() string {
	return r.fs.Name()
}

func (r *RealFs) Chmod(name string, mode os.FileMode) error {
	return r.fs.Chmod(name, mode)
}

func (r *RealFs) Chown(name string, uid, gid int) error {
	return r.fs.Chown(name, uid, gid)
}

func (r *RealFs) Chtimes(name string, atime time.Time, mtime time.Time) error {
	return r.fs.Chtimes(name, atime, mtime)
}

func (r *RealFs) Rename(oldname, newname string) error {
	return r.fs.Rename(oldname, newname)
}

func (r *RealFs) Create(name string) (afero.File, error) {
	return r.fs.Create(name)
}

func (r *RealFs) Mkdir(name string, perm os.FileMode) error {
	return r.fs.Mkdir(name, perm)
}

func (r *RealFs) Open(name string) (afero.File, error) {
	return r.fs.Open(name)
}

func (r *RealFs) RLock(ctx context.Context, filename string) (func(), error) {
	lockFile, err := lockPath(filename)
	if err != nil {
		return nil, err
	}

	fileLock := flock.New(lockFile)

	locked, err := fileLock.TryRLockContext(ctx, time.Second)
	if err != nil {
		return nil, fmt.Errorf("try rlock: %w", err)
	}

	if !locked {
		return nil, errors.New("acquire rlock on file")
	}

	return func() {
		if err := fileLock.Unlock(); err != nil {
			// TODO(zhuravlev): log errors
			panic(err)
		}
	}, nil
}

func (r *RealFs) Lock(ctx context.Context, filename string) (func(), error) {
	lockFile, err := lockPath(filename)
	if err != nil {
		return nil, err
	}

	fileLock := flock.New(lockFile)

	locked, err := fileLock.TryLockContext(ctx, time.Second)
	if err != nil {
		return nil, fmt.Errorf("try lock: %w", err)
	}

	if !locked {
		return nil, errors.New("acquire lock on file")
	}

	return func() {
		if err := fileLock.Unlock(); err != nil {
			// TODO(zhuravlev): log error
			panic(err)
		}
	}, nil
}

// lockPath returns the file that is locked on behalf of filename.
//
// The data file itself must not be locked: on Windows flock is a mandatory lock, so the holder
// could not write the file through another handle. A file next to the data file would show up in
// users' repositories, so the lock files live in the user cache dir, named after the data file's path.
func lockPath(filename string) (string, error) {
	abs, err := filepath.Abs(filename)
	if err != nil {
		return "", fmt.Errorf("abs path (%s): %w", filename, err)
	}

	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs) // paths are case-insensitive there
	}

	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}

	dir = filepath.Join(dir, "toolset-locks")
	if err := os.MkdirAll(dir, DefaultDirPerm); err != nil {
		return "", fmt.Errorf("create lock dir: %w", err)
	}

	sum := sha256.Sum256([]byte(abs))

	return filepath.Join(dir, hex.EncodeToString(sum[:16])+".lock"), nil
}
