package workdir

import (
	"context"
	"runtime"
	"testing"

	"github.com/kazhuravlev/optional"
	"github.com/kazhuravlev/toolset/internal/fsh"
	"github.com/kazhuravlev/toolset/internal/link"
	"github.com/kazhuravlev/toolset/internal/workdir/structs"
	"github.com/stretchr/testify/require"
)

func TestLink_NothingToLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip for Windows")
	}

	ctx := context.Background()
	fs := fsh.NewMemFS(nil)
	require.NoError(t, Init(ctx, fs, "/dir"))

	wd, err := New(ctx, fs, "/dir")
	require.NoError(t, err)

	res, err := wd.Link(ctx, "/bin-dir", false)
	require.NoError(t, err)
	require.Empty(t, res)
}

func TestToLinkTools(t *testing.T) {
	t.Parallel()

	installed := func(module, name, bin string, alias optional.Val[string]) structs.ToolState {
		return structs.ToolState{
			Module: structs.ModuleInfo{Name: name, BinPath: bin, IsInstalled: true},
			Tool:   structs.Tool{Module: module, Alias: alias},
		}
	}

	t.Run("name is the alias, or the binary name", func(t *testing.T) {
		t.Parallel()

		got, err := toLinkTools([]structs.ToolState{
			installed("mod/a", "a", "/cache/a", optional.Empty[string]()),
			installed("mod/b", "b", "/cache/b", optional.New("lint")),
			installed("img", "img", "", optional.New("hadolint")),
		})
		require.NoError(t, err)
		require.Equal(t, []link.Tool{
			{Name: "a", Binary: "/cache/a"},
			{Name: "lint", Binary: "/cache/b", Alias: "lint"},
			{Name: "hadolint", Alias: "hadolint"},
		}, got)
	})

	t.Run("a tool that is not installed is an error naming all of them", func(t *testing.T) {
		t.Parallel()

		missing := func(module string) structs.ToolState {
			return structs.ToolState{Tool: structs.Tool{Module: module}}
		}

		got, err := toLinkTools([]structs.ToolState{
			installed("mod/a", "a", "/cache/a", optional.Empty[string]()),
			missing("mod/m1"),
			missing("mod/m2"),
		})
		require.Nil(t, got)
		require.ErrorContains(t, err, "run `toolset sync` first: mod/m1, mod/m2")
	})
}
