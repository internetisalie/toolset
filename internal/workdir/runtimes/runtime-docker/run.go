package runtimedocker

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
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
	TTY       bool   // stdin and stdout are terminals
	Untrusted bool   // tool comes from a remote spec
}

func currentHostEnv() hostEnv {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()

	return hostEnv{
		Cwd:  cwd,
		Home: home,
		UID:  os.Getuid(),
		GID:  os.Getgid(),
		// -t merges stderr into stdout and rewrites line endings, so it is only safe when both ends are
		// terminals. Not os.ModeCharDevice: /dev/null is one.
		TTY: term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())),
	}
}

// ValidateConfig performs the checks that need neither the filesystem nor run-time variables.
// It is called when a tool is added, and again (together with path checks) before every run.
//
// Untrusted tools (not in the local spec) are held to a stricter profile, because their author
// is not the person running them: no network, no host env variables, no named volumes and
// read-only host mounts.
func ValidateConfig(cfg structs.DockerConfig, untrusted bool) error {
	if _, err := validateNetwork(cfg.Network, untrusted); err != nil {
		return err
	}

	if err := validateEnv(cfg.Env, untrusted); err != nil {
		return err
	}

	for _, spec := range cfg.Volumes {
		m, err := parseVolume(spec)
		if err != nil {
			return fmt.Errorf("volume %q: %w", spec, err)
		}

		if err := checkUntrustedMount(m, untrusted); err != nil {
			return fmt.Errorf("volume %q: %w", spec, err)
		}
	}

	return nil
}

// buildRunArgs returns the arguments of `docker run` (without the leading "docker").
// It validates the config; any violation is returned as an error and nothing must be run.
func buildRunArgs(cfg structs.DockerConfig, imgRef string, env hostEnv, userArgs []string) ([]string, error) {
	if err := ValidateConfig(cfg, env.Untrusted); err != nil {
		return nil, err
	}

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

	network, _ := validateNetwork(cfg.Network, env.Untrusted) // validated above
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

	// Any network other than none reaches the LAN, the docker host or other containers.
	if untrusted && network != defaultNetwork {
		return "", fmt.Errorf("network %q is not allowed for tools from remote specs (only %q)", network, defaultNetwork)
	}

	return network, nil
}

func validateEnv(names []string, untrusted bool) error {
	if untrusted && len(names) > 0 {
		return errors.New("passing host env variables is not allowed for tools from remote specs")
	}

	for _, name := range names {
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("invalid env name: %q", name)
		}
	}

	return nil
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

// parseVolume splits src:dst[:opts] without expanding variables or touching the filesystem.
func parseVolume(spec string) (mount, error) {
	return parseVolumeFor(spec, runtime.GOOS == "windows")
}

// hasDriveLetter reports whether s starts with a Windows drive ("C:\" or "C:/").
func hasDriveLetter(s string) bool {
	if len(s) < 3 || s[1] != ':' || (s[2] != '\\' && s[2] != '/') {
		return false
	}

	c := s[0] | 0x20 // to lower case
	return c >= 'a' && c <= 'z'
}

// isHostSource reports whether src is (or expands to) a host path, as opposed to a named volume.
func isHostSource(src string, windows bool) bool {
	if strings.HasPrefix(src, "/") || strings.HasPrefix(src, "$") {
		return true
	}

	// On Linux "c:/x" is a named volume "c" mounted at /x, so drives are a Windows-only notion.
	return windows && (hasDriveLetter(src) || strings.HasPrefix(src, `\`))
}

func parseVolumeFor(spec string, windows bool) (mount, error) {
	// The colon of a drive letter is not a field separator.
	drive, rest := "", spec
	if windows && hasDriveLetter(spec) {
		drive, rest = spec[:2], spec[2:]
	}

	parts := strings.Split(rest, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return mount{}, errors.New("expected src:dst[:ro|rw]")
	}

	parts[0] = drive + parts[0]

	m := mount{src: parts[0], dst: parts[1]}
	if len(parts) == 3 {
		m.opts = parts[2]
		if m.opts != "ro" && m.opts != "rw" {
			return mount{}, fmt.Errorf("unsupported option %q (allowed: ro, rw)", m.opts)
		}
	}

	// The container side is always a Linux path.
	if !path.IsAbs(m.dst) {
		return mount{}, errors.New("container path must be absolute")
	}

	if !isHostSource(m.src, windows) {
		if !netNameRe.MatchString(m.src) {
			return mount{}, fmt.Errorf("invalid volume source %q", m.src)
		}

		m.named = true
	}

	return m, nil
}

// checkUntrustedMount applies the extra rules for tools from remote specs.
func checkUntrustedMount(m mount, untrusted bool) error {
	if !untrusted {
		return nil
	}

	if m.named {
		// A named volume can be shared with other projects (or be bound to a host path by its driver).
		return errors.New("named volumes are not allowed for tools from remote specs")
	}

	if m.opts == "rw" {
		return errors.New("read-write mounts are not allowed for tools from remote specs")
	}

	return nil
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
	m, err := parseVolume(spec)
	if err != nil {
		return mount{}, err
	}

	if err := checkUntrustedMount(m, env.Untrusted); err != nil {
		return mount{}, err
	}

	if m.named {
		return m, nil
	}

	m.src = expandVars(m.src, env)
	if !filepath.IsAbs(m.src) {
		return mount{}, fmt.Errorf("source %q does not expand to an absolute path", m.src)
	}

	if env.Untrusted {
		// The project mount is writable by default, which would let a remote tool rewrite
		// .toolset.json, the lock file or .git/hooks.
		m.opts = "ro"
	}

	resolved, err := filepath.EvalSymlinks(m.src)
	if err != nil {
		// A missing source would be silently created by docker as a root-owned directory.
		return mount{}, fmt.Errorf("resolve source: %w", err)
	}

	// A volume root ("/" or `C:\`) would expose the whole filesystem.
	if filepath.Dir(resolved) == resolved || filepath.Base(resolved) == "docker.sock" {
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
