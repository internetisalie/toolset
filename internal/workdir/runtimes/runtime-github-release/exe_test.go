package runtimegh

import (
	"archive/zip"
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/kazhuravlev/optional"
	"github.com/stretchr/testify/require"

	"github.com/kazhuravlev/toolset/internal/fsh"
	"github.com/kazhuravlev/toolset/internal/workdir/structs"
)

func Test_GetModule_windowsBinaryHasExe(t *testing.T) {
	binary := filepath.Join("/tools", "gh", "golangci/golangci-lint@v1.61.0", "golangci-lint.exe")

	t.Run("windows", func(t *testing.T) {
		memFS := fsh.NewMemFS(map[string]string{binary: "binary"})
		rt := New(memFS, "/tools", nil, goosWindows, goarchAMD64)

		mod, err := rt.GetModule(context.Background(), "golangci/golangci-lint@v1.61.0")
		require.NoError(t, err)
		require.Equal(t, binary, mod.BinPath)
		require.True(t, mod.IsInstalled)
		require.Equal(t, "golangci-lint", mod.Name, "the tool keeps its plain name")
	})

	t.Run("other systems", func(t *testing.T) {
		rt := New(fsh.NewMemFS(nil), "/tools", nil, goosLinux, goarchAMD64)

		mod, err := rt.GetModule(context.Background(), "golangci/golangci-lint@v1.61.0")
		require.NoError(t, err)
		require.Equal(t, filepath.Join("/tools", "gh", "golangci/golangci-lint@v1.61.0", "golangci-lint"), mod.BinPath)
		require.False(t, mod.IsInstalled)
	})
}

func Test_findBinary_windowsArchive(t *testing.T) {
	memFS := fsh.NewMemFS(map[string]string{
		"/extracted/golangci-lint-1.61.0-windows-amd64/golangci-lint.exe": "binary",
		"/extracted/golangci-lint-1.61.0-windows-amd64/README.md":         "readme",
	})
	rt := New(memFS, "/tools", nil, goosWindows, goarchAMD64)

	got, err := rt.findBinary("/extracted", fsh.ExeName(goosWindows, "golangci-lint"))
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash("/extracted/golangci-lint-1.61.0-windows-amd64/golangci-lint.exe"), got)

	// Looking for the name without the suffix is what failed on Windows.
	_, err = rt.findBinary("/extracted", "golangci-lint")
	require.Error(t, err)
}

// zipOf makes a zip archive holding the given files.
func zipOf(t *testing.T, files map[string]string) string {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	for name, body := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)

		_, err = w.Write([]byte(body))
		require.NoError(t, err)
	}

	require.NoError(t, zw.Close())

	return buf.String()
}

// The release of a Windows tool holds <name>.exe. Installing it must find that file and report the tool installed.
func Test_Install_windowsRelease(t *testing.T) {
	archive := zipOf(t, map[string]string{
		"golangci-lint-1.61.0-windows-amd64/golangci-lint.exe": "binary",
		"golangci-lint-1.61.0-windows-amd64/README.md":         "readme",
	})

	rt := New(fsh.NewRealFS(), t.TempDir(), nil, goosWindows, goarchAMD64)
	rt.gh = &testGithubClient{assetName: "golangci-lint-1.61.0-windows-amd64.zip", content: archive}

	const module = "golangci/golangci-lint@v1.61.0"

	require.NoError(t, rt.Install(context.Background(), module, optional.Empty[structs.Pin]()))

	mod, err := rt.GetModule(context.Background(), module)
	require.NoError(t, err)
	require.Equal(t, "golangci-lint.exe", filepath.Base(mod.BinPath))
	require.True(t, mod.IsInstalled)
}
