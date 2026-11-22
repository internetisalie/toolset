package runtimedocker

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"

	"github.com/kazhuravlev/toolset/internal/workdir/structs"
)

const (
	defaultVolume  = "$PROJECT:/work"
	defaultNetwork = "none"
)

var (
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	netNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// hostEnv is everything about the host that influences the docker command line.
type hostEnv struct {
	Project   string // $PROJECT
	Cwd       string // $PWD
	Home      string // $HOME
	UID, GID  int    // -1 disables --user
	TTY       bool   // stdin is a terminal
	Untrusted bool   // tool comes from a remote spec
	Getenv    func(string) string
}

func currentHostEnv() hostEnv {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()

	return hostEnv{
		Cwd:    cwd,
		Home:   home,
		UID:    os.Getuid(),
		GID:    os.Getgid(),
		TTY:    term.IsTerminal(int(os.Stdin.Fd())), // not ModeCharDevice: /dev/null is one
		Getenv: os.Getenv,
	}
}

// buildRunArgs returns the arguments of `docker run` (without the leading "docker").
// It validates the config; any violation is returned as an error and nothing must be run.
func buildRunArgs(cfg structs.DockerConfig, imgRef string, env hostEnv, userArgs []string) ([]string, error) {
	project, err := filepath.EvalSymlinks(env.Project)
	if err != nil {
		return nil, fmt.Errorf("resolve project root (%s): %w", env.Project, err)
	}

	args := []string{"run", "--rm", "-i"}
	if env.TTY {
		args = append(args, "-t")
	}

	if env.UID >= 0 && env.GID >= 0 {
		args = append(args, "--user", fmt.Sprintf("%d:%d", env.UID, env.GID))
	}

	args = append(args, "--cap-drop", "ALL", "--security-opt", "no-new-privileges")

	network, err := validateNetwork(cfg.Network, env.Untrusted)
	if err != nil {
		return nil, err
	}

	args = append(args, "--network", network)

	volumes := cfg.Volumes
	if len(volumes) == 0 {
		volumes = []string{defaultVolume}
	}

	var projectMount string // container path where the project root is mounted
	for _, spec := range volumes {
		mount, err := resolveVolume(spec, env, project)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", spec, err)
		}

		if mount.src == project && projectMount == "" {
			projectMount = mount.dst
		}

		args = append(args, "-v", mount.String())
	}

	switch {
	case cfg.Workdir != "":
		args = append(args, "-w", cfg.Workdir)
	case projectMount != "":
		args = append(args, "-w", containerCwd(projectMount, project, env.Cwd))
	}

	for _, name := range cfg.Env {
		if !envNameRe.MatchString(name) {
			return nil, fmt.Errorf("invalid env name: %q", name)
		}

		// "-e NAME" (without a value) passes the variable only when it is set on the host.
		args = append(args, "-e", name)
	}

	if cfg.Entrypoint != "" {
		args = append(args, "--entrypoint", cfg.Entrypoint)
	}

	args = append(args, imgRef)
	args = append(args, cfg.Args...)
	args = append(args, userArgs...)

	return args, nil
}

func validateNetwork(network string, untrusted bool) (string, error) {
	if network == "" {
		return defaultNetwork, nil
	}

	if !netNameRe.MatchString(network) {
		return "", fmt.Errorf("invalid network: %q", network)
	}

	if network == "host" && untrusted {
		return "", errors.New("network 'host' is not allowed for tools from remote specs")
	}

	return network, nil
}

type mount struct {
	src, dst, opts string
	named          bool // src is a named volume, not a host path
}

func (m mount) String() string {
	s := m.src + ":" + m.dst
	if m.opts != "" {
		s += ":" + m.opts
	}

	return s
}

func expandVars(s string, env hostEnv) string {
	return strings.NewReplacer(
		"${PROJECT}", env.Project, "$PROJECT", env.Project,
		"${PWD}", env.Cwd, "$PWD", env.Cwd,
		"${HOME}", env.Home, "$HOME", env.Home,
	).Replace(s)
}

// resolveVolume expands variables and checks that a host path is allowed to be mounted.
func resolveVolume(spec string, env hostEnv, project string) (mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return mount{}, errors.New("expected src:dst[:opts]")
	}

	m := mount{src: expandVars(parts[0], env), dst: parts[1]}
	if len(parts) == 3 {
		m.opts = parts[2]
		for _, o := range strings.Split(m.opts, ",") {
			if o != "ro" && o != "rw" {
				return mount{}, fmt.Errorf("unsupported option %q (allowed: ro, rw)", o)
			}
		}
	}

	if !path.IsAbs(m.dst) {
		return mount{}, errors.New("container path must be absolute")
	}

	if !strings.HasPrefix(m.src, "/") {
		// Named volume: it never touches the host filesystem.
		if !netNameRe.MatchString(m.src) {
			return mount{}, fmt.Errorf("invalid volume source %q", m.src)
		}

		m.named = true

		return m, nil
	}

	resolved, err := filepath.EvalSymlinks(m.src)
	if err != nil {
		// A missing source would be silently created by docker as a root-owned directory.
		return mount{}, fmt.Errorf("resolve source: %w", err)
	}

	if resolved == "/" || filepath.Base(resolved) == "docker.sock" {
		return mount{}, fmt.Errorf("mounting %s is not allowed", resolved)
	}

	allowed := []string{project}
	if !env.Untrusted && env.Home != "" {
		if home, err := filepath.EvalSymlinks(env.Home); err == nil {
			allowed = append(allowed, home)
		}
	}

	if !underAny(resolved, allowed) {
		return mount{}, fmt.Errorf("%s is outside of the allowed directories (%s)", resolved, strings.Join(allowed, ", "))
	}

	m.src = resolved

	return m, nil
}

func underAny(p string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// containerCwd maps the host cwd into the container, assuming the project root is mounted at projectMount.
func containerCwd(projectMount, project, cwd string) string {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return projectMount
	}

	rel, err := filepath.Rel(project, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return projectMount
	}

	return path.Join(projectMount, filepath.ToSlash(rel))
}
