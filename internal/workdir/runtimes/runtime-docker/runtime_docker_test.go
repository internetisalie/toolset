package runtimedocker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kazhuravlev/optional"
	"github.com/stretchr/testify/require"

	"github.com/kazhuravlev/toolset/internal/workdir/structs"
)

type call struct{ args []string }

// fakeCmd records calls. Output fails for any call whose joined args start with a key in failOn.
type fakeCmd struct {
	calls       []call
	outputs     map[string]string
	failOn      map[string]bool
	passthrough error
}

func (f *fakeCmd) Output(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, call{args})
	key := strings.Join(args, " ")

	for prefix := range f.failOn {
		if strings.HasPrefix(key, prefix) {
			return "", errors.New("boom")
		}
	}

	for prefix, out := range f.outputs {
		if strings.HasPrefix(key, prefix) {
			return out, nil
		}
	}

	return "", nil
}

func (f *fakeCmd) Passthrough(_ context.Context, args ...string) error {
	f.calls = append(f.calls, call{append([]string{"<run>"}, args...)})
	return f.passthrough
}

func (f *fakeCmd) joined() []string {
	res := make([]string, len(f.calls))
	for i, c := range f.calls {
		res[i] = strings.Join(c.args, " ")
	}

	return res
}

func newTest(f *fakeCmd, env hostEnv) *Runtime { return &Runtime{cmd: f, env: env} }

func TestParseImage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    image
		wantErr bool
	}{
		{in: "ghcr.io/a/b@v1", want: image{"ghcr.io/a/b", "v1"}},
		{in: "ghcr.io/a/b:v1", want: image{"ghcr.io/a/b", "v1"}},
		{in: "alpine", want: image{"alpine", "latest"}},
		{in: "localhost:5000/x", want: image{"localhost:5000/x", "latest"}},
		{in: "localhost:5000/x:2", want: image{"localhost:5000/x", "2"}},
		{in: "localhost:5000/x@2", want: image{"localhost:5000/x", "2"}},
		{in: "", wantErr: true},
		{in: "a/b@sha256:abc", wantErr: true},
		{in: "a/b:1@2", wantErr: true},
		{in: "a b@1", wantErr: true},
		{in: "a/b@", wantErr: true},
		{in: "--volume=/:/host@ro", wantErr: true},
		{in: "-x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := parseImage(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestRuntime_Parse(t *testing.T) {
	t.Parallel()

	f := &fakeCmd{}
	got, err := newTest(f, hostEnv{}).Parse(context.Background(), "ghcr.io/a/b:v1")
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/a/b@v1", got)
	require.Equal(t, []string{"manifest inspect ghcr.io/a/b:v1"}, f.joined())

	f = &fakeCmd{failOn: map[string]bool{"manifest": true}}
	_, err = newTest(f, hostEnv{}).Parse(context.Background(), "ghcr.io/a/b:v1")
	require.Error(t, err)
}

func TestRuntime_GetModule(t *testing.T) {
	t.Parallel()

	f := &fakeCmd{}
	mod, err := newTest(f, hostEnv{}).GetModule(context.Background(), "ghcr.io/a/b@v1")
	require.NoError(t, err)
	require.Equal(t, "b", mod.Name)
	require.True(t, mod.IsInstalled)
	require.Equal(t, "ghcr.io/a/b@v1", mod.Mod.S())
	require.Empty(t, mod.BinPath)

	f = &fakeCmd{failOn: map[string]bool{"image inspect": true}}
	mod, err = newTest(f, hostEnv{}).GetModule(context.Background(), "ghcr.io/a/b@v1")
	require.NoError(t, err)
	require.False(t, mod.IsInstalled)
}

func TestRuntime_GetPin(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("a", 64)

	// Real output: the top-level digest comes first; per-platform manifests follow and must be ignored.
	other := "sha256:" + strings.Repeat("e", 64)
	out := "Name:      docker.io/library/b:v1\nMediaType: application/vnd.oci.image.index.v1+json\nDigest:    " + digest +
		"\n           \nManifests: \n  Name:        docker.io/library/b:v1@" + other + "\n  Digest:      " + other + "\n"

	f := &fakeCmd{outputs: map[string]string{"buildx imagetools inspect": out}}
	pin, err := newTest(f, hostEnv{}).GetPin(context.Background(), "a/b@v1")
	require.NoError(t, err)
	require.Equal(t, digest, pin.Val().ImageDigest)

	// Only indented (per-platform) Digest lines, no top-level one: must not be accepted.
	nested := "Name: x\nManifests:\n  Name: y\n  Digest:      " + other + "\n"
	f = &fakeCmd{outputs: map[string]string{"buildx imagetools inspect": nested}}
	_, err = newTest(f, hostEnv{}).GetPin(context.Background(), "a/b@v1")
	require.Error(t, err)

	f = &fakeCmd{outputs: map[string]string{"buildx imagetools inspect": "garbage"}}
	_, err = newTest(f, hostEnv{}).GetPin(context.Background(), "a/b@v1")
	require.Error(t, err)

	f = &fakeCmd{failOn: map[string]bool{"buildx": true}}
	_, err = newTest(f, hostEnv{}).GetPin(context.Background(), "a/b@v1")
	require.Error(t, err)
}

func TestRuntime_Install(t *testing.T) {
	t.Parallel()

	f := &fakeCmd{}
	require.NoError(t, newTest(f, hostEnv{}).Install(context.Background(), "a/b@v1", optional.Empty[structs.Pin]()))
	require.Equal(t, []string{"pull --quiet a/b:v1"}, f.joined())

	digest := "sha256:" + strings.Repeat("b", 64)
	f = &fakeCmd{}
	require.NoError(t, newTest(f, hostEnv{}).Install(context.Background(), "a/b@v1", optional.New(structs.Pin{ImageDigest: digest})))
	require.Equal(t, []string{
		"pull --quiet a/b@" + digest,
		"tag a/b@" + digest + " a/b:v1",
	}, f.joined())

	f = &fakeCmd{failOn: map[string]bool{"pull": true}}
	require.Error(t, newTest(f, hostEnv{}).Install(context.Background(), "a/b@v1", optional.Empty[structs.Pin]()))
}

func TestRuntime_RunTool(t *testing.T) {
	t.Parallel()

	env, _ := testEnv(t)
	digest := "sha256:" + strings.Repeat("c", 64)
	tool := structs.Tool{Runtime: "docker", Module: "a/b@v1", Pin: optional.New(structs.Pin{ImageDigest: digest})}

	t.Run("pinned runs by digest", func(t *testing.T) {
		t.Parallel()

		f := &fakeCmd{}
		require.NoError(t, newTest(f, env).RunTool(context.Background(), tool, RunOptions{ProjectRoot: env.Project}, "x"))

		last := f.joined()[len(f.calls)-1]
		require.Contains(t, last, " a/b@"+digest+" x")
		require.True(t, strings.HasPrefix(last, "<run> run "))
	})

	t.Run("not installed", func(t *testing.T) {
		t.Parallel()

		f := &fakeCmd{failOn: map[string]bool{"image inspect": true}}
		err := newTest(f, env).RunTool(context.Background(), tool, RunOptions{ProjectRoot: env.Project})
		require.ErrorIs(t, err, structs.ErrToolNotInstalled)
	})

	t.Run("exit code", func(t *testing.T) {
		t.Parallel()

		f := &fakeCmd{passthrough: structs.RunError{ExitCode: 3}}
		err := newTest(f, env).RunTool(context.Background(), tool, RunOptions{ProjectRoot: env.Project})

		var runErr structs.RunError
		require.ErrorAs(t, err, &runErr)
		require.Equal(t, 3, runErr.ExitCode)
	})

	t.Run("invalid config is not run", func(t *testing.T) {
		t.Parallel()

		bad := tool
		bad.Docker = optional.New(structs.DockerConfig{Volumes: []string{"/:/h"}})

		f := &fakeCmd{}
		require.Error(t, newTest(f, env).RunTool(context.Background(), bad, RunOptions{ProjectRoot: env.Project}))

		for _, c := range f.joined() {
			require.False(t, strings.HasPrefix(c, "<run>"))
		}
	})

	t.Run("untrusted blocks host network", func(t *testing.T) {
		t.Parallel()

		host := tool
		host.Docker = optional.New(structs.DockerConfig{Network: "host"})

		f := &fakeCmd{}
		require.Error(t, newTest(f, env).RunTool(context.Background(), host, RunOptions{ProjectRoot: env.Project, Untrusted: true}))
	})
}

func TestRuntime_Remove(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("d", 64)
	f := &fakeCmd{}
	tool := structs.Tool{Module: "a/b@v1", Pin: optional.New(structs.Pin{ImageDigest: digest})}
	require.NoError(t, newTest(f, hostEnv{}).Remove(context.Background(), tool))
	require.Equal(t, []string{"rmi a/b:v1", "rmi a/b@" + digest}, f.joined())
}

func TestRuntime_GetLatest_Unsupported(t *testing.T) {
	t.Parallel()

	got, upd, err := newTest(&fakeCmd{}, hostEnv{}).GetLatest(context.Background(), "a/b@v1")
	require.NoError(t, err)
	require.False(t, upd)
	require.Equal(t, "a/b@v1", got)
}
