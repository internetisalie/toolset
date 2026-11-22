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
		Getenv:  func(string) string { return "" },
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
		{name: "root mount", cfg: structs.DockerConfig{Volumes: []string{"/:/host"}}},
		{name: "docker socket", cfg: structs.DockerConfig{Volumes: []string{"/var/run/docker.sock:/s"}}},
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
