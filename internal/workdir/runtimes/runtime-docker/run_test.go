package runtimedocker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kazhuravlev/toolset/internal/workdir/structs"
)

func testEnv(t *testing.T) (hostEnv, string) {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(project, "sub"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "home", "cache"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "outside"), 0o755))

	return hostEnv{
		Project: project,
		Cwd:     project,
		Home:    filepath.Join(root, "home"),
		UID:     1000,
		GID:     1001,
	}, root
}

func TestBuildRunArgs_Defaults(t *testing.T) {
	t.Parallel()

	env, _ := testEnv(t)

	got, err := buildRunArgs(structs.DockerConfig{}, "img:1", env, []string{"a", "b"})
	require.NoError(t, err)
	require.Equal(t, []string{
		"run", "--rm", "-i",
		"--user", "1000:1001",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--network", "none",
		"-v", env.Project + ":/work",
		"-w", "/work",
		"img:1", "a", "b",
	}, got)
}

func TestBuildRunArgs_Full(t *testing.T) {
	t.Parallel()

	env, _ := testEnv(t)
	env.TTY = true
	env.Cwd = filepath.Join(env.Project, "sub")

	got, err := buildRunArgs(structs.DockerConfig{
		Entrypoint: "/bin/x",
		Args:       []string{"--fixed"},
		Volumes:    []string{"$PROJECT:/work", "$HOME/cache:/cache:ro", "named-cache:/n"},
		Network:    "bridge",
		Env:        []string{"CI"},
	}, "img@sha256:abc", env, []string{"file"})
	require.NoError(t, err)
	require.Equal(t, []string{
		"run", "--rm", "-i", "-t",
		"--user", "1000:1001",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--network", "bridge",
		"-v", env.Project + ":/work",
		"-v", env.Home + "/cache:/cache:ro",
		"-v", "named-cache:/n",
		"-w", "/work/sub",
		"-e", "CI",
		"--entrypoint", "/bin/x",
		"img@sha256:abc", "--fixed", "file",
	}, got)
}

func TestBuildRunArgs_ExplicitWorkdirAndNoUser(t *testing.T) {
	t.Parallel()

	env, _ := testEnv(t)
	env.UID, env.GID = -1, -1

	got, err := buildRunArgs(structs.DockerConfig{Workdir: "/app"}, "img:1", env, nil)
	require.NoError(t, err)
	require.NotContains(t, got, "--user")
	require.Contains(t, got, "/app")
}

func TestBuildRunArgs_Rejects(t *testing.T) {
	t.Parallel()

	env, root := testEnv(t)

	require.NoError(t, os.Symlink(filepath.Join(root, "outside"), filepath.Join(env.Project, "escape")))

	tests := []struct {
		name      string
		cfg       structs.DockerConfig
		untrusted bool
	}{
		{name: "outside project and home", cfg: structs.DockerConfig{Volumes: []string{root + "/outside:/o"}}},
		{name: "symlink escape", cfg: structs.DockerConfig{Volumes: []string{"$PROJECT/escape:/o"}}},
		{name: "missing source", cfg: structs.DockerConfig{Volumes: []string{"$PROJECT/nope:/o"}}},
		{name: "relative container path", cfg: structs.DockerConfig{Volumes: []string{"$PROJECT:work"}}},
		{name: "bad option", cfg: structs.DockerConfig{Volumes: []string{"$PROJECT:/w:suid"}}},
		{name: "malformed volume", cfg: structs.DockerConfig{Volumes: []string{"justone"}}},
		{name: "bad network", cfg: structs.DockerConfig{Network: "container:abc"}},
		{name: "host network untrusted", cfg: structs.DockerConfig{Network: "host"}, untrusted: true},
		{name: "home mount untrusted", cfg: structs.DockerConfig{Volumes: []string{"$HOME/cache:/c"}}, untrusted: true},
		{name: "bad env name", cfg: structs.DockerConfig{Env: []string{"A=b"}}},
		{name: "unexpanded non-absolute source", cfg: structs.DockerConfig{Volumes: []string{"$NOPE/x:/o"}}},
		{name: "bridge network untrusted", cfg: structs.DockerConfig{Network: "bridge"}, untrusted: true},
		{name: "named network untrusted", cfg: structs.DockerConfig{Network: "myapp_default"}, untrusted: true},
		{name: "env untrusted", cfg: structs.DockerConfig{Env: []string{"GITHUB_TOKEN"}}, untrusted: true},
		{name: "named volume untrusted", cfg: structs.DockerConfig{Volumes: []string{"shared-db:/d"}}, untrusted: true},
		{name: "rw mount untrusted", cfg: structs.DockerConfig{Volumes: []string{"$PROJECT:/w:rw"}}, untrusted: true},
		{name: "combined options", cfg: structs.DockerConfig{Volumes: []string{"$PROJECT:/w:ro,rw"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := env
			e.Untrusted = tt.untrusted

			_, err := buildRunArgs(tt.cfg, "img:1", e, nil)
			require.Error(t, err)
		})
	}
}

func TestBuildRunArgs_HostNetworkTrusted(t *testing.T) {
	t.Parallel()

	env, _ := testEnv(t)

	got, err := buildRunArgs(structs.DockerConfig{Network: "host"}, "img:1", env, nil)
	require.NoError(t, err)
	require.Contains(t, got, "host")
}

// The next two tests mount paths that ARE inside the allowed directories, so the only thing that can
// reject them is the dedicated check. (A path outside project/home is rejected by the directory check
// anyway, which would make these pass for the wrong reason.)

func TestBuildRunArgs_RejectsDockerSocketInsideProject(t *testing.T) {
	t.Parallel()

	env, _ := testEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.Project, "docker.sock"), nil, 0o600))

	_, err := buildRunArgs(structs.DockerConfig{Volumes: []string{"$PROJECT/docker.sock:/s"}}, "img:1", env, nil)
	require.ErrorContains(t, err, "not allowed")
}

func TestBuildRunArgs_RejectsRootWhenHomeIsRoot(t *testing.T) {
	t.Parallel()

	env, _ := testEnv(t)
	env.Home = "/"

	_, err := buildRunArgs(structs.DockerConfig{Volumes: []string{"/:/host"}}, "img:1", env, nil)
	require.ErrorContains(t, err, "not allowed")
}

func TestBuildRunArgs_UntrustedProfile(t *testing.T) {
	t.Parallel()

	env, _ := testEnv(t)
	env.Untrusted = true

	got, err := buildRunArgs(structs.DockerConfig{}, "img:1", env, nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		"run", "--rm", "-i",
		"--user", "1000:1001",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--network", "none",
		"-v", env.Project + ":/work:ro",
		"-w", "/work",
		"img:1",
	}, got)

	// An explicit read-only mount is fine too.
	got, err = buildRunArgs(structs.DockerConfig{Volumes: []string{"$PROJECT:/src:ro"}, Network: "none"}, "img:1", env, nil)
	require.NoError(t, err)
	require.Contains(t, got, env.Project+":/src:ro")
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	// Static checks only: nothing here touches the filesystem, so it can run when a tool is added.
	require.NoError(t, ValidateConfig(structs.DockerConfig{}, false))
	require.NoError(t, ValidateConfig(structs.DockerConfig{
		Volumes: []string{"$PROJECT:/work", "$HOME/c:/c:ro", "cache:/n"},
		Network: "bridge",
		Env:     []string{"CI"},
	}, false))

	for name, cfg := range map[string]structs.DockerConfig{
		"missing dst":    {Volumes: []string{"justone"}},
		"relative dst":   {Volumes: []string{"$PROJECT:work"}},
		"bad option":     {Volumes: []string{"$PROJECT:/w:suid"}},
		"bad network":    {Network: "container:x"},
		"bad env":        {Env: []string{"A=B"}},
		"bad named vol":  {Volumes: []string{"-x:/w"}},
		"empty src name": {Volumes: []string{":/w"}},
	} {
		require.Error(t, ValidateConfig(cfg, false), name)
	}

	require.NoError(t, ValidateConfig(structs.DockerConfig{Volumes: []string{"$PROJECT:/work:ro"}, Network: "none"}, true))

	for name, cfg := range map[string]structs.DockerConfig{
		"bridge":       {Network: "bridge"},
		"host":         {Network: "host"},
		"env":          {Env: []string{"HOME"}},
		"named volume": {Volumes: []string{"cache:/c"}},
		"rw":           {Volumes: []string{"$PROJECT:/w:rw"}},
	} {
		require.Error(t, ValidateConfig(cfg, true), name)
	}
}
