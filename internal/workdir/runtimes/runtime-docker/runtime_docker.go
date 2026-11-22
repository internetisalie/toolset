// Package runtimedocker runs tools from container images by shelling out to the docker CLI.
//
// Module format is "<image>@<tag>" (e.g. ghcr.io/hadolint/hadolint@v2.12.0), which keeps the
// same name@version shape as the other runtimes. The image digest, when pinned, lives in structs.Pin.
package runtimedocker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"github.com/kazhuravlev/optional"

	"github.com/kazhuravlev/toolset/internal/prog"
	"github.com/kazhuravlev/toolset/internal/workdir/structs"
)

const runtimeName = "docker"

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// digestLineRe finds the top-level "Digest:" line of `docker buildx imagetools inspect`.
// The plain output is parsed because --format is not honored by every buildx build.
var digestLineRe = regexp.MustCompile(`(?m)^Digest:\s+(sha256:[0-9a-f]{64})\s*$`)

// commander abstracts the docker CLI so tests do not need a daemon.
type commander interface {
	// Output runs docker and returns trimmed stdout. Stderr is part of the returned error.
	Output(ctx context.Context, args ...string) (string, error)
	// Passthrough runs docker with the process stdio attached.
	// A non-zero exit is returned as structs.RunError.
	Passthrough(ctx context.Context, args ...string) error
}

// RunOptions describe the environment of a single tool run.
type RunOptions struct {
	// ProjectRoot is the directory that $PROJECT expands to.
	ProjectRoot string
	// Untrusted is true when the tool does not come from the local spec (remote/include).
	// Untrusted tools get a stricter sandbox (no host network, project-only mounts).
	Untrusted bool
}

type Runtime struct {
	cmd commander
	env hostEnv
}

func New() *Runtime {
	return &Runtime{cmd: execCommander{}, env: currentHostEnv()}
}

// Discover returns the docker runtime when the docker binary is on PATH.
func Discover(_ context.Context) ([]*Runtime, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, nil //nolint:nilerr // docker is optional
	}

	return []*Runtime{New()}, nil
}

type image struct {
	Name string // ghcr.io/hadolint/hadolint
	Tag  string // v2.12.0
}

func (i image) tagRef() string { return i.Name + ":" + i.Tag }

func (i image) digestRef(digest string) string { return i.Name + "@" + digest }

func (i image) module() string { return i.Name + "@" + i.Tag }

func parseImage(str string) (image, error) {
	str = strings.TrimSpace(str)
	if str == "" {
		return image{}, errors.New("image not provided")
	}

	name, tag, hasAt := strings.Cut(str, "@")
	if !hasAt {
		// Be friendly: accept the docker-style "image:tag". A colon before the last slash is a registry port.
		if idx := strings.LastIndex(str, ":"); idx > strings.LastIndex(str, "/") {
			name, tag = str[:idx], str[idx+1:]
		} else {
			name, tag = str, "latest"
		}
	}

	if strings.HasPrefix(tag, "sha256:") {
		return image{}, errors.New("digest references are not accepted here: use a tag and --pin")
	}

	if name == "" || tag == "" || strings.ContainsAny(name+tag, " \t\n@") {
		return image{}, fmt.Errorf("invalid image reference: %q", str)
	}

	if strings.Contains(name[strings.LastIndex(name, "/")+1:], ":") {
		return image{}, fmt.Errorf("invalid image reference: %q (put the tag after '@')", str)
	}

	return image{Name: name, Tag: tag}, nil
}

// ref returns the reference to use for pull/run: by digest when pinned, otherwise by tag.
func ref(img image, pin optional.Val[structs.Pin]) string {
	if p, ok := pin.Get(); ok && p.ImageDigest != "" {
		return img.digestRef(p.ImageDigest)
	}

	return img.tagRef()
}

// Parse normalizes the image reference to name@tag and checks that it exists in the registry.
func (r *Runtime) Parse(ctx context.Context, str string) (string, error) {
	img, err := parseImage(str)
	if err != nil {
		return "", err
	}

	if _, err := r.cmd.Output(ctx, "manifest", "inspect", img.tagRef()); err != nil {
		return "", fmt.Errorf("inspect image (%s): %w", img.tagRef(), err)
	}

	return img.module(), nil
}

func (r *Runtime) GetModule(ctx context.Context, module string) (*structs.ModuleInfo, error) {
	img, err := parseImage(module)
	if err != nil {
		return nil, fmt.Errorf("parse module (%s): %w", module, err)
	}

	_, inspectErr := r.cmd.Output(ctx, "image", "inspect", "--format", "{{.Id}}", img.tagRef())

	return &structs.ModuleInfo{
		Name: path.Base(img.Name),
		Mod:  prog.NewVer(img.Name, img.Tag),
		// NOTE: images live in the docker daemon, there is no binary on disk.
		BinDir:      "",
		BinPath:     "",
		IsInstalled: inspectErr == nil,
		IsPrivate:   false,
	}, nil
}

// GetPin resolves the tag to the manifest digest without pulling the image.
func (r *Runtime) GetPin(ctx context.Context, program string) (optional.Val[structs.Pin], error) {
	img, err := parseImage(program)
	if err != nil {
		return optional.Empty[structs.Pin](), fmt.Errorf("parse module (%s): %w", program, err)
	}

	out, err := r.cmd.Output(ctx, "buildx", "imagetools", "inspect", img.tagRef())
	if err != nil {
		return optional.Empty[structs.Pin](), fmt.Errorf("resolve digest (%s): %w", img.tagRef(), err)
	}

	m := digestLineRe.FindStringSubmatch(out)
	if m == nil || !digestRe.MatchString(m[1]) {
		return optional.Empty[structs.Pin](), fmt.Errorf("digest not found in imagetools output for %s", img.tagRef())
	}

	return optional.New(structs.Pin{ImageDigest: m[1]}), nil
}

// Install pulls the image. When pinned it pulls by digest (docker verifies the content against it)
// and then tags it, so GetModule can see it as installed.
func (r *Runtime) Install(ctx context.Context, program string, pin optional.Val[structs.Pin]) error {
	img, err := parseImage(program)
	if err != nil {
		return fmt.Errorf("parse module (%s): %w", program, err)
	}

	pullRef := ref(img, pin)
	if _, err := r.cmd.Output(ctx, "pull", "--quiet", pullRef); err != nil {
		return fmt.Errorf("docker pull (%s): %w", pullRef, err)
	}

	if pullRef != img.tagRef() {
		if _, err := r.cmd.Output(ctx, "tag", pullRef, img.tagRef()); err != nil {
			return fmt.Errorf("docker tag (%s): %w", pullRef, err)
		}
	}

	return nil
}

// Run runs the image with default settings in the current directory.
func (r *Runtime) Run(ctx context.Context, program string, args ...string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}

	return r.RunTool(ctx, structs.Tool{Runtime: runtimeName, Module: program}, RunOptions{ProjectRoot: cwd}, args...)
}

// RunTool runs the tool using its docker settings.
func (r *Runtime) RunTool(ctx context.Context, tool structs.Tool, opts RunOptions, args ...string) error {
	img, err := parseImage(tool.Module)
	if err != nil {
		return fmt.Errorf("parse module (%s): %w", tool.Module, err)
	}

	imgRef := ref(img, tool.Pin)
	if _, err := r.cmd.Output(ctx, "image", "inspect", "--format", "{{.Id}}", imgRef); err != nil {
		return fmt.Errorf("program (%s) is not installed: %w", tool.Module, structs.ErrToolNotInstalled)
	}

	env := r.env
	env.Project = opts.ProjectRoot
	env.Untrusted = opts.Untrusted

	dockerArgs, err := buildRunArgs(tool.Docker.ValDefault(structs.DockerConfig{}), imgRef, env, args)
	if err != nil {
		return fmt.Errorf("build docker args (%s): %w", tool.Module, err)
	}

	if err := r.cmd.Passthrough(ctx, dockerArgs...); err != nil {
		var runErr structs.RunError
		if errors.As(err, &runErr) {
			return fmt.Errorf("exit not ok (%s): %w", tool.Module, err)
		}

		return fmt.Errorf("run (%s): %w", tool.Module, err)
	}

	return nil
}

// GetLatest is not supported: the docker CLI cannot list registry tags.
// Use `toolset ensure docker <image>@<tag>` to move to a new version.
func (r *Runtime) GetLatest(_ context.Context, module string) (string, bool, error) {
	return module, false, nil
}

func (r *Runtime) Remove(ctx context.Context, tool structs.Tool) error {
	img, err := parseImage(tool.Module)
	if err != nil {
		return fmt.Errorf("parse module (%s): %w", tool.Module, err)
	}

	if _, err := r.cmd.Output(ctx, "rmi", img.tagRef()); err != nil {
		return fmt.Errorf("docker rmi (%s): %w", img.tagRef(), err)
	}

	if p, ok := tool.Pin.Get(); ok && p.ImageDigest != "" {
		// Best effort: the digest reference may already be gone together with the tag.
		_, _ = r.cmd.Output(ctx, "rmi", img.digestRef(p.ImageDigest))
	}

	return nil
}

func (r *Runtime) Version() string { return runtimeName }

type execCommander struct{}

func (execCommander) Output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}

func (execCommander) Passthrough(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return errors.Join(structs.RunError{ExitCode: exitErr.ExitCode()}, err)
		}

		return err
	}

	return nil
}
