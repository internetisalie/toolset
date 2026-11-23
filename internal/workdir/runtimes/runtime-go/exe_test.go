package runtimego

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func Test_GetModule_windowsBinaryHasExe(t *testing.T) {
	const module = "golang.org/x/tools/cmd/stringer@v0.53.0"

	rt := newTestRuntimeWithHTTP(t, nil)

	rt.goos = "linux"

	mod, err := rt.GetModule(context.Background(), module)
	require.NoError(t, err)
	require.Equal(t, "stringer", filepath.Base(mod.BinPath))

	rt.goos = "windows"

	mod, err = rt.GetModule(context.Background(), module)
	require.NoError(t, err)
	require.Equal(t, "stringer.exe", filepath.Base(mod.BinPath))
	require.Equal(t, "stringer", mod.Name, "the tool keeps its plain name")

	// A tool is installed when the file `go install` wrote is there.
	require.False(t, mod.IsInstalled)
	require.NoError(t, rt.fs.MkdirAll(mod.BinDir, 0o755))
	require.NoError(t, afero.WriteFile(rt.fs, mod.BinPath, []byte("binary"), 0o755))

	mod, err = rt.GetModule(context.Background(), module)
	require.NoError(t, err)
	require.True(t, mod.IsInstalled)
}
