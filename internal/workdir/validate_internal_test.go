package workdir

import (
	"testing"

	"github.com/kazhuravlev/optional"
	"github.com/stretchr/testify/require"

	"github.com/kazhuravlev/toolset/internal/workdir/structs"
)

func TestValidateTool(t *testing.T) {
	t.Parallel()

	cfg := optional.New(structs.DockerConfig{Network: "none"})

	tests := []struct {
		name    string
		tool    structs.Tool
		wantErr bool
	}{
		{name: "go tool", tool: structs.Tool{Runtime: "go@1.24", Module: "a@v1"}},
		{name: "docker with alias", tool: structs.Tool{Runtime: "docker", Module: "a@v1", Alias: optional.New("a")}},
		{name: "docker with alias and config", tool: structs.Tool{Runtime: "docker", Module: "a@v1", Alias: optional.New("a"), Docker: cfg}},
		{name: "docker without alias", tool: structs.Tool{Runtime: "docker", Module: "a@v1"}, wantErr: true},
		{name: "docker config on go", tool: structs.Tool{Runtime: "go", Module: "a@v1", Docker: cfg}, wantErr: true},
		{name: "docker config on gh", tool: structs.Tool{Runtime: "gh", Module: "a/b@v1", Docker: cfg}, wantErr: true},
		{name: "docker bad network", tool: dockerTool(structs.DockerConfig{Network: "-x"}), wantErr: true},
		{name: "docker bad env", tool: dockerTool(structs.DockerConfig{Env: []string{"A B"}}), wantErr: true},
		{name: "docker bad volume", tool: dockerTool(structs.DockerConfig{Volumes: []string{"nonsense"}}), wantErr: true},
		{name: "docker relative container path", tool: dockerTool(structs.DockerConfig{Volumes: []string{"$PROJECT:work"}}), wantErr: true},
		{name: "docker good volume", tool: dockerTool(structs.DockerConfig{Volumes: []string{"$PROJECT:/work:ro"}})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateTool(tt.tool)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
		})
	}
}

func dockerTool(cfg structs.DockerConfig) structs.Tool {
	return structs.Tool{Runtime: "docker", Module: "a@v1", Alias: optional.New("a"), Docker: optional.New(cfg)}
}

func TestIsLocalTool(t *testing.T) {
	t.Parallel()

	local := dockerTool(structs.DockerConfig{Network: "none"})
	pinned := local
	pinned.Pin = optional.New(structs.Pin{ImageDigest: "sha256:" + "a"}) // digest content is irrelevant here

	tests := []struct {
		name string
		spec structs.Tools
		tool structs.Tool
		want bool
	}{
		{name: "identical", spec: structs.Tools{local}, tool: local, want: true},
		{name: "empty spec", spec: nil, tool: local, want: false},
		{name: "other tool", spec: structs.Tools{local}, tool: structs.Tool{Runtime: "docker", Module: "b@v1", Alias: optional.New("b")}, want: false},
		{
			name: "same ID, remote asks for host network",
			spec: structs.Tools{local},
			tool: dockerTool(structs.DockerConfig{Network: "host"}),
			want: false,
		},
		{
			name: "same ID, remote adds a mount",
			spec: structs.Tools{local},
			tool: dockerTool(structs.DockerConfig{Network: "none", Volumes: []string{"$HOME:/h"}}),
			want: false,
		},
		{name: "same ID, different pin", spec: structs.Tools{local}, tool: pinned, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, isLocalTool(tt.spec, tt.tool))
		})
	}
}

func TestInheritDockerSettings(t *testing.T) {
	t.Parallel()

	existing := structs.Tool{
		Runtime: "docker",
		Module:  "img@v1",
		Alias:   optional.New("img"),
		Tags:    []string{"lint"},
		Docker:  optional.New(structs.DockerConfig{Entrypoint: "/bin/img", Network: "none"}),
	}
	specTools := structs.Tools{existing}

	t.Run("bare ensure with a new version keeps everything", func(t *testing.T) {
		t.Parallel()

		got := inheritDockerSettings(specTools, structs.Tool{Runtime: "docker", Module: "img@v2"})
		require.Equal(t, existing.Alias, got.Alias)
		require.Equal(t, existing.Docker, got.Docker)
		require.Equal(t, existing.Tags, got.Tags)
		require.Equal(t, "img@v2", got.Module)
		require.NoError(t, validateTool(got))
	})

	t.Run("explicit values win", func(t *testing.T) {
		t.Parallel()

		want := structs.Tool{
			Runtime: "docker",
			Module:  "img@v2",
			Alias:   optional.New("other"),
			Tags:    []string{"x"},
			Docker:  optional.New(structs.DockerConfig{Network: "bridge"}),
		}
		require.Equal(t, want, inheritDockerSettings(specTools, want))
	})

	t.Run("unknown tool is untouched", func(t *testing.T) {
		t.Parallel()

		tool := structs.Tool{Runtime: "docker", Module: "new@v1"}
		require.Equal(t, tool, inheritDockerSettings(specTools, tool))
	})

	t.Run("other runtimes are untouched", func(t *testing.T) {
		t.Parallel()

		tool := structs.Tool{Runtime: "go", Module: "img@v2"}
		require.Equal(t, tool, inheritDockerSettings(specTools, tool))
	})
}
