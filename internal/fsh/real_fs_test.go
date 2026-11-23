package fsh_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kazhuravlev/toolset/internal/fsh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type SampleJson struct {
	Field string `json:"field"`
}

func TestIsExists(t *testing.T) {
	fs := fsh.NewMemFS(map[string]string{
		"/foo/bar": "foo",
	})
	require.False(t, fsh.IsExists(fs, "/not/exists/path"))
	require.True(t, fsh.IsExists(fs, "/foo/bar"))
}

func TestReadJson(t *testing.T) {
	ctx := context.Background()

	t.Run("error_on_file_not_exists", func(t *testing.T) {
		fs := fsh.NewMemFS(nil)
		res, err := fsh.ReadJson[SampleJson](ctx, fs, "/not/exists/path")
		require.Error(t, err)
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Nil(t, res)
	})

	t.Run("not_a_json_file", func(t *testing.T) {
		fs := fsh.NewMemFS(map[string]string{
			"/data.xml": "<xml></xml>",
		})

		res, err := fsh.ReadJson[SampleJson](ctx, fs, "/data.xml")
		require.Error(t, err)
		require.Nil(t, res)
	})

	t.Run("valid_file", func(t *testing.T) {
		fs := fsh.NewMemFS(nil)
		input := SampleJson{Field: "value"}
		require.NoError(t, fsh.WriteJson(ctx, fs, input, "/data.json"))

		res, err := fsh.ReadJson[SampleJson](ctx, fs, "/data.json")
		require.NoError(t, err)
		require.Equal(t, input, *res)
	})
}

func TestWriteJson(t *testing.T) {
	ctx := context.Background()

	t.Run("write_new_file_and_rewrite_it", func(t *testing.T) {
		fs := fsh.NewMemFS(nil)
		input := SampleJson{Field: "value"}
		require.NoError(t, fsh.WriteJson(ctx, fs, input, "/data.json"))
		require.NoError(t, fsh.WriteJson(ctx, fs, input, "/data.json"))
		require.NoError(t, fsh.WriteJson(ctx, fs, input, "/data.json"))
	})

	t.Run("bad_input_structure", func(t *testing.T) {
		fs := fsh.NewMemFS(nil)
		input := map[struct{}]int{{}: 1}
		require.Error(t, fsh.WriteJson(ctx, fs, input, "/data.json"))
	})
}

func TestReadOrCreateJson(t *testing.T) {
	ctx := context.Background()

	t.Run("auto_create_file_when_not_exists", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("skip for Windows")
		}

		fs := fsh.NewMemFS(nil)

		// 1. file not exists
		require.False(t, fsh.IsExists(fs, "/data.json"))

		// 2. auto-create file with default content
		res, err := fsh.ReadOrCreateJson[SampleJson](ctx, fs, "/data.json", SampleJson{Field: "default-value"})
		require.NoError(t, err)
		require.Equal(t, SampleJson{Field: "default-value"}, *res)

		// 3. file exists
		require.True(t, fsh.IsExists(fs, "/data.json"))

		// 4. content is equal to written before
		res, err = fsh.ReadOrCreateJson[SampleJson](ctx, fs, "/data.json", SampleJson{Field: "another-default"})
		require.NoError(t, err)
		require.Equal(t, SampleJson{Field: "default-value"}, *res)
	})
}

func TestLocks(t *testing.T) {
	isolateLockDir(t)

	ctx := context.Background()
	fs := fsh.NewRealFS()

	// Use a temp file per test to avoid cross-test interference.
	lockFile := func(t *testing.T) string {
		t.Helper()
		f, err := os.CreateTemp("", "fsh-lock-test-*.lock")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		t.Cleanup(func() { os.Remove(f.Name()) }) //nolint:errcheck
		return f.Name()
	}

	t.Run("auto_create_file_when_not_exists", func(t *testing.T) {
		filename := filepath.Join(os.TempDir(), "fsh-test-autocreate.lock")
		os.Remove(filename) //nolint:errcheck
		t.Cleanup(func() { os.Remove(filename) }) //nolint:errcheck

		unlock, err := fs.Lock(ctx, filename)
		require.NoError(t, err)
		unlock()
	})

	t.Run("held_lock_blocks_concurrent_locker", func(t *testing.T) {
		filename := lockFile(t)

		unlock, err := fs.Lock(ctx, filename)
		require.NoError(t, err)
		defer unlock()

		// A lock attempt with an immediately-cancelled context must fail:
		// the file is held so the lock cannot be acquired.
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel() // cancel immediately

		_, err = fs.Lock(cancelCtx, filename)
		require.Error(t, err, "second Lock must fail while first is held")
	})

	t.Run("unlock_allows_re_lock", func(t *testing.T) {
		filename := lockFile(t)

		unlock, err := fs.Lock(ctx, filename)
		require.NoError(t, err)
		unlock() // release

		// After unlock the file must be acquirable again.
		// Bound the wait so the test fails fast rather than hanging until the global timeout.
		const acquireTimeout = 5 * time.Second
		lockCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
		defer cancel()

		unlock2, err := fs.Lock(lockCtx, filename)
		require.NoError(t, err)
		unlock2()
	})
}

// isolateLockDir keeps the lock files of a test out of the real user cache dir.
func isolateLockDir(t *testing.T) {
	t.Helper()

	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LocalAppData", cache)
	t.Setenv("HOME", cache)
}

// Locks must not be taken on the data file: on Windows a locked file cannot be written through
// another handle, and a lock file next to it would litter the user's repository.
func TestLocks_DoNotTouchDataFile(t *testing.T) {
	isolateLockDir(t)

	ctx := context.Background()
	fs := fsh.NewRealFS()
	dir := t.TempDir()
	data := filepath.Join(dir, ".toolset.json")

	t.Run("lock_does_not_create_the_data_file", func(t *testing.T) {
		unlock, err := fs.Lock(ctx, data)
		require.NoError(t, err)
		defer unlock()

		require.NoFileExists(t, data)

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Empty(t, entries, "nothing may appear next to the data file")
	})

	t.Run("data_file_is_writable_while_locked", func(t *testing.T) {
		unlock, err := fs.Lock(ctx, data)
		require.NoError(t, err)
		defer unlock()

		f, err := fs.OpenFile(data, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = f.WriteString("{}")
		require.NoError(t, err)
		require.NoError(t, f.Close())
	})

	t.Run("write_and_read_json_round_trip", func(t *testing.T) {
		path := filepath.Join(dir, "sample.json")

		require.NoError(t, fsh.WriteJson(ctx, fs, SampleJson{Field: "v"}, path))

		res, err := fsh.ReadJson[SampleJson](ctx, fs, path)
		require.NoError(t, err)
		require.Equal(t, SampleJson{Field: "v"}, *res)
	})

	t.Run("same_file_different_spelling_excludes", func(t *testing.T) {
		unlock, err := fs.Lock(ctx, data)
		require.NoError(t, err)
		defer unlock()

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		_, err = fs.Lock(cancelCtx, filepath.Join(dir, ".", "sub", "..", ".toolset.json"))
		require.Error(t, err, "a different spelling of the same path must hit the same lock")
	})

	t.Run("different_files_do_not_block_each_other", func(t *testing.T) {
		unlock, err := fs.Lock(ctx, data)
		require.NoError(t, err)
		defer unlock()

		unlock2, err := fs.Lock(ctx, filepath.Join(dir, ".toolset.lock.json"))
		require.NoError(t, err)
		unlock2()
	})
}
