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
