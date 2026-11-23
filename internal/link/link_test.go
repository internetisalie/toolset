package link_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kazhuravlev/toolset/internal/fsh"
	"github.com/kazhuravlev/toolset/internal/link"
)

var unix = link.Params{GOOS: "linux", Toolset: "/usr/bin/toolset", Project: "/work/proj", CacheDir: "/cache"}
var windows = link.Params{GOOS: "windows", Toolset: `C:\bin\toolset.exe`, Project: `C:\work\proj`, CacheDir: `C:\cache`}

func TestPlan_Unix(t *testing.T) {
	t.Parallel()

	got, err := link.Plan(unix, []link.Tool{
		{Name: "lint", Binary: "/cache/bin/golangci-lint"},
		{Name: "hadolint", Alias: "hadolint"},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)

	// sorted by name
	require.Equal(t, "hadolint", got[0].Name)
	require.Equal(t, link.KindShell, got[0].Kind)
	require.Equal(t, "#!/bin/sh\n"+
		"# toolset-link project=/work/proj\n"+
		"exec '/usr/bin/toolset' --dir '/work/proj' run 'hadolint' \"$@\"\n", got[0].Content)

	require.Equal(t, link.Entry{Name: "lint", Kind: link.KindSymlink, Target: "/cache/bin/golangci-lint"}, got[1])
}

func TestPlan_Windows(t *testing.T) {
	t.Parallel()

	got, err := link.Plan(windows, []link.Tool{
		{Name: "lint.exe", Binary: `C:\cache\bin\golangci-lint.exe`},
		{Name: "hadolint", Alias: "hadolint"},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, "hadolint.cmd", got[0].Name)
	require.Equal(t, link.KindCmd, got[0].Kind)
	require.Equal(t, "@echo off\r\n"+
		"REM toolset-link project=C:\\work\\proj\r\n"+
		`"C:\bin\toolset.exe" --dir "C:\work\proj" run hadolint %*`+"\r\n"+
		"exit /b %ERRORLEVEL%\r\n", got[0].Content)

	// The .exe of the binary name is dropped: the entry is lint.cmd, and it calls the binary.
	require.Equal(t, "lint.cmd", got[1].Name)
	require.Contains(t, got[1].Content, `"C:\cache\bin\golangci-lint.exe" %*`)
}

func TestPlan_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params link.Params
		tools  []link.Tool
		want   string
	}{
		{name: "path separator", params: unix, tools: []link.Tool{{Name: "../x", Binary: "/b"}}, want: "cannot be linked"},
		{name: "leading dash", params: unix, tools: []link.Tool{{Name: "-x", Binary: "/b"}}, want: "cannot be linked"},
		{name: "empty", params: unix, tools: []link.Tool{{Name: "", Binary: "/b"}}, want: "cannot be linked"},
		{name: "space", params: unix, tools: []link.Tool{{Name: "a b", Binary: "/b"}}, want: "cannot be linked"},
		{name: "no binary and no alias", params: unix, tools: []link.Tool{{Name: "x"}}, want: "no binary"},
		{name: "unsafe alias in a shim", params: unix, tools: []link.Tool{{Name: "x", Alias: "a;rm"}}, want: "not usable"},
		{
			name:   "duplicate name",
			params: unix,
			tools:  []link.Tool{{Name: "x", Binary: "/a"}, {Name: "x", Alias: "x"}},
			want:   "both want the name",
		},
		{
			name:   "duplicate name differing in case and extension on windows",
			params: windows,
			tools:  []link.Tool{{Name: "Tool.exe", Binary: `C:\a`}, {Name: "tool", Binary: `C:\b`}},
			want:   "both want the name",
		},
		{
			name:   "quote in a windows path",
			params: windows,
			tools:  []link.Tool{{Name: "x", Binary: `C:\a"b`}},
			want:   "cannot be used in a .cmd file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := link.Plan(tt.params, tt.tools)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestPlan_Quoting(t *testing.T) {
	t.Parallel()

	p := unix
	p.Project = "/work/it's here"
	p.Toolset = "/opt/my tools/toolset"

	got, err := link.Plan(p, []link.Tool{{Name: "x", Alias: "x"}})
	require.NoError(t, err)
	require.Contains(t, got[0].Content, `exec '/opt/my tools/toolset' --dir '/work/it'\''s here' run 'x' "$@"`)

	// A percent sign must reach cmd as a literal one.
	w := windows
	w.Project = `C:\100%\proj`

	got, err = link.Plan(w, []link.Tool{{Name: "x", Alias: "x"}})
	require.NoError(t, err)
	require.Contains(t, got[0].Content, `--dir "C:\100%%\proj"`)

	// The project path must not be able to end the comment line it is written into.
	p.Project = "/a\nexec evil"
	got, err = link.Plan(p, []link.Tool{{Name: "x", Alias: "x"}})
	require.NoError(t, err)
	require.NotContains(t, strings.Split(got[0].Content, "\n")[2], "evil\n#")
	require.Equal(t, "# toolset-link project=/a exec evil", strings.Split(got[0].Content, "\n")[1])
}

// env is a real directory tree: a fake cache with an installed binary, and an empty target dir.
type env struct {
	fs     fsh.FS
	cache  string
	bin    string // an installed binary inside the cache
	dir    string // the target dir
	params link.Params
}

func newEnv(t *testing.T) env {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks are not created on Windows")
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	e := env{
		fs:    fsh.NewRealFS(),
		cache: filepath.Join(root, "cache"),
		dir:   filepath.Join(root, "bin"),
	}
	e.bin = filepath.Join(e.cache, "tool-1.0")

	require.NoError(t, os.MkdirAll(e.cache, 0o755))
	require.NoError(t, os.WriteFile(e.bin, []byte("binary"), 0o755))

	e.params = link.Params{
		GOOS:     runtime.GOOS,
		Toolset:  "/usr/bin/toolset",
		Project:  filepath.Join(root, "proj"),
		CacheDir: e.cache,
	}

	return e
}

func (e env) plan(t *testing.T, tools ...link.Tool) []link.Entry {
	t.Helper()

	entries, err := link.Plan(e.params, tools)
	require.NoError(t, err)

	return entries
}

func ops(results []link.Result) map[string]string {
	m := make(map[string]string, len(results))
	for _, r := range results {
		m[r.Name] = r.Op
	}

	return m
}

func TestApply_CreatesAndIsIdempotent(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	entries := e.plan(t, link.Tool{Name: "tool", Binary: e.bin}, link.Tool{Name: "img", Alias: "img"})

	res, err := link.Apply(e.fs, e.dir, e.params, entries, false)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"tool": link.OpLink, "img": link.OpLink}, ops(res))

	target, err := os.Readlink(filepath.Join(e.dir, "tool"))
	require.NoError(t, err)
	require.Equal(t, e.bin, target)

	shim, err := os.ReadFile(filepath.Join(e.dir, "img"))
	require.NoError(t, err)
	require.Contains(t, string(shim), "run 'img'")

	info, err := os.Stat(filepath.Join(e.dir, "img"))
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&0o100, "a shim must be executable")

	res, err = link.Apply(e.fs, e.dir, e.params, entries, false)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"tool": link.OpUnchanged, "img": link.OpUnchanged}, ops(res))
}

func TestApply_RefusesForeignFilesAndChangesNothing(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	require.NoError(t, os.MkdirAll(e.dir, 0o755))

	// "b" is somebody's file, "c" is somebody's symlink, "d" is a directory; "a" would be fine.
	require.NoError(t, os.WriteFile(filepath.Join(e.dir, "b"), []byte("mine"), 0o755))
	require.NoError(t, os.Symlink("/usr/bin/env", filepath.Join(e.dir, "c")))
	require.NoError(t, os.Mkdir(filepath.Join(e.dir, "d"), 0o755))

	entries := e.plan(t,
		link.Tool{Name: "a", Binary: e.bin},
		link.Tool{Name: "b", Binary: e.bin},
		link.Tool{Name: "c", Binary: e.bin},
		link.Tool{Name: "d", Alias: "d"},
	)

	_, err := link.Apply(e.fs, e.dir, e.params, entries, false)
	require.Error(t, err)
	require.ErrorContains(t, err, "nothing was changed")

	for _, name := range []string{"b", "c", "d"} {
		require.ErrorContains(t, err, filepath.Join(e.dir, name))
	}

	require.NoFileExists(t, filepath.Join(e.dir, "a"), "a valid entry must not be created when another one is refused")

	got, err := os.ReadFile(filepath.Join(e.dir, "b"))
	require.NoError(t, err)
	require.Equal(t, "mine", string(got))
}

func TestApply_ReplacesOwnEntries(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	require.NoError(t, os.MkdirAll(e.dir, 0o755))

	other := filepath.Join(e.cache, "tool-2.0")
	require.NoError(t, os.WriteFile(other, []byte("binary 2"), 0o755))

	// A symlink to an older cached binary is ours, and gets re-pointed.
	require.NoError(t, os.Symlink(e.bin, filepath.Join(e.dir, "tool")))

	// A symlink into the cache that must become a shim: the binary must not be overwritten through it.
	require.NoError(t, os.Symlink(e.bin, filepath.Join(e.dir, "img")))

	entries := e.plan(t, link.Tool{Name: "tool", Binary: other}, link.Tool{Name: "img", Alias: "img"})

	_, err := link.Apply(e.fs, e.dir, e.params, entries, false)
	require.NoError(t, err)

	target, err := os.Readlink(filepath.Join(e.dir, "tool"))
	require.NoError(t, err)
	require.Equal(t, other, target)

	binary, err := os.ReadFile(e.bin)
	require.NoError(t, err)
	require.Equal(t, "binary", string(binary), "writing the shim went through the symlink into the binary")

	shim, err := os.Lstat(filepath.Join(e.dir, "img"))
	require.NoError(t, err)
	require.Zero(t, shim.Mode()&os.ModeSymlink)

	// An old shim is rewritten when it differs.
	require.NoError(t, os.WriteFile(filepath.Join(e.dir, "img"), []byte("#!/bin/sh\n# toolset-link project=/old\n"), 0o755))

	res, err := link.Apply(e.fs, e.dir, e.params, entries, false)
	require.NoError(t, err)
	require.Equal(t, link.OpLink, ops(res)["img"])
}

func TestApply_Prune(t *testing.T) {
	t.Parallel()

	e := newEnv(t)

	all := e.plan(t,
		link.Tool{Name: "keep", Binary: e.bin},
		link.Tool{Name: "gone-link", Binary: e.bin},
		link.Tool{Name: "gone-shim", Alias: "gone-shim"},
		link.Tool{Name: "taken-over", Binary: e.bin},
	)

	_, err := link.Apply(e.fs, e.dir, e.params, all, false)
	require.NoError(t, err)

	// Another project shares the dir.
	otherProject := e.params
	otherProject.Project = filepath.Join(filepath.Dir(e.params.Project), "other")

	_, err = link.Apply(e.fs, e.dir, otherProject, e.plan(t, link.Tool{Name: "theirs", Binary: e.bin}), false)
	require.NoError(t, err)

	// The user replaced one of ours with their own file after linking.
	require.NoError(t, os.Remove(filepath.Join(e.dir, "taken-over")))
	require.NoError(t, os.WriteFile(filepath.Join(e.dir, "taken-over"), []byte("user's"), 0o755))

	// Without --prune nothing is removed.
	only := e.plan(t, link.Tool{Name: "keep", Binary: e.bin})

	res, err := link.Apply(e.fs, e.dir, e.params, only, false)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"keep": link.OpUnchanged}, ops(res))
	require.FileExists(t, filepath.Join(e.dir, "gone-shim"))

	// With --prune the ones made by this project are removed, and nothing else.
	res, err = link.Apply(e.fs, e.dir, e.params, only, true)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"keep":       link.OpUnchanged,
		"gone-link":  link.OpRemove,
		"gone-shim":  link.OpRemove,
		"taken-over": link.OpSkip,
	}, ops(res))

	require.NoFileExists(t, filepath.Join(e.dir, "gone-link"))
	require.NoFileExists(t, filepath.Join(e.dir, "gone-shim"))
	require.FileExists(t, filepath.Join(e.dir, "taken-over"), "a file the user put there must stay")
	require.FileExists(t, filepath.Join(e.dir, "theirs"), "another project's entry must stay")

	// The skipped file is forgotten, so the next prune does not report it again.
	res, err = link.Apply(e.fs, e.dir, e.params, only, true)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"keep": link.OpUnchanged}, ops(res))
}

func TestApply_WindowsShadowing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tool.exe"), []byte("x"), 0o755))

	// The entries are plain files, so this runs on any host.
	entries, err := link.Plan(windows, []link.Tool{{Name: "tool", Binary: `C:\cache\tool.exe`}, {Name: "free", Alias: "free"}})
	require.NoError(t, err)

	_, err = link.Apply(fsh.NewRealFS(), dir, windows, entries, false)
	require.ErrorContains(t, err, "would run instead of")
	require.NoFileExists(t, filepath.Join(dir, "free.cmd"))
	require.NoFileExists(t, filepath.Join(dir, "tool.cmd"))
}

func TestApply_WindowsShims(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	entries, err := link.Plan(windows, []link.Tool{{Name: "img", Alias: "img"}})
	require.NoError(t, err)

	res, err := link.Apply(fsh.NewRealFS(), dir, windows, entries, false)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"img.cmd": link.OpLink}, ops(res))

	got, err := os.ReadFile(filepath.Join(dir, "img.cmd"))
	require.NoError(t, err)
	require.Equal(t, entries[0].Content, string(got))

	res, err = link.Apply(fsh.NewRealFS(), dir, windows, entries, false)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"img.cmd": link.OpUnchanged}, ops(res))
}
