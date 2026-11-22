package structs

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kazhuravlev/optional"
	"github.com/kazhuravlev/toolset/internal/prog"
)

var ErrToolNotInstalled = errors.New("tool not installed")

// PinnedAsset holds the expected SHA256 digest for a single platform binary archive.
type PinnedAsset struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Name   string `json:"name"`
	Digest string `json:"digest"` // "sha256:<hex64>" from asset.GetDigest()
}

// Pin holds the supply-chain verification data for a tool.
// Exactly one field is populated per runtime:
//   - go-runtime  → CommitHash (from proxy.golang.org Origin.Hash)
//   - gh-runtime  → Assets (per-platform SHA256 archive digests)
//   - docker      → ImageDigest (image manifest digest, "sha256:<hex64>")
type Pin struct {
	CommitHash  string        `json:"commit_hash,omitempty"`
	Assets      []PinnedAsset `json:"assets,omitempty"`
	ImageDigest string        `json:"image_digest,omitempty"`
}

// DockerConfig describes how a docker-runtime tool is executed.
// All fields are optional; see runtime-docker for defaults.
type DockerConfig struct {
	// Entrypoint overrides the image ENTRYPOINT.
	Entrypoint string `json:"entrypoint,omitempty"`
	// Args are placed before the user's arguments.
	Args []string `json:"args,omitempty"`
	// Volumes in `docker run -v` syntax. $PROJECT, $PWD and $HOME are expanded at run time.
	Volumes []string `json:"volumes,omitempty"`
	// Workdir inside the container.
	Workdir string `json:"workdir,omitempty"`
	// Network is one of none (default), bridge, host or a named network.
	Network string `json:"network,omitempty"`
	// Env lists host environment variable names passed through to the container.
	Env []string `json:"env,omitempty"`
}

type RunError struct {
	ExitCode int
}

func (e RunError) Error() string { return strconv.Itoa(e.ExitCode) }

type Tool struct {
	// Name of runtime
	Runtime string `json:"runtime"`
	// Path to module with version
	Module string `json:"module"`
	// Alias create a link in tools. Works like exposing some tools
	Alias optional.Val[string] `json:"alias"`
	Tags  []string             `json:"tags"`
	// Pin holds supply-chain verification data. Empty for unpinned tools.
	Pin optional.Val[Pin] `json:"pin"`
	// Docker holds execution settings. Used by the docker runtime only.
	Docker optional.Val[DockerConfig] `json:"docker"`
}

func (t Tool) ID() string {
	return fmt.Sprintf("%s:%s", t.Runtime, t.Module)
}

func (t Tool) ModuleName() string {
	return strings.Split(t.Module, "@")[0]
}

func (t Tool) RuntimeName() string {
	return strings.Split(t.Runtime, "@")[0]
}

// IsSame returns true when it detects that this is the same tools. It does not check tool version.
func (t Tool) IsSame(tool Tool) bool {
	if t.RuntimeName() != tool.RuntimeName() { // go != js
		return false
	}

	m1 := t.ModuleName()
	m2 := tool.ModuleName()

	return m1 == m2
}

type Tools []Tool

// Add will add tool to list if that tool not exists yet. Returns true when tool was added.
func (tools *Tools) Add(tool Tool) bool {
	for _, t := range *tools {
		if t.IsSame(tool) {
			return false
		}
	}

	*tools = append(*tools, tool)

	return true
}

// UpsertTool adds the tool if it does not exist, or replaces it if it does.
// Returns true if the tool was newly added, false if an existing entry was updated.
func (tools *Tools) UpsertTool(tool Tool) bool {
	for i, t := range *tools {
		if t.IsSame(tool) {
			(*tools)[i] = tool
			return false
		}
	}

	*tools = append(*tools, tool)
	return true
}

func (tools *Tools) Filter(tags []string) Tools {
	if len(tags) == 0 {
		return *tools
	}

	res := make(Tools, 0)

	for _, t := range *tools {
		isTarget := slices.ContainsFunc(t.Tags, func(tag string) bool {
			return slices.Contains(tags, tag)
		})
		if !isTarget {
			continue
		}

		res = append(res, t)
	}

	return res
}

func (tools *Tools) Remove(tool Tool) bool {
	for i, t := range *tools {
		if t.IsSame(tool) {
			*tools = slices.Delete(*tools, i, i+1)
			return true
		}
	}

	return false
}

type ModuleInfo struct {
	Name        string // golangci-lint
	Mod         prog.Version
	BinDir      string // /home/user/bin/tools/.golangci-lint__v1.1.1
	BinPath     string // /home/user/bin/tools/.golangci-lint__v1.1.1/golangci-lint
	IsInstalled bool
	IsPrivate   bool
}

type Spec struct {
	// This dir is store all toolset-related files.
	// This directory should be managed by toolset only.
	// Deprecated: do not use this field. All tools stored into global cache directory.
	Dir      string    `json:"dir,omitempty"`
	Tools    Tools     `json:"tools"`
	Includes []Include `json:"includes"`
}

func (s *Spec) AddInclude(include Include) bool {
	for _, inc := range s.Includes {
		if inc.IsSame(include) {
			return false
		}
	}

	s.Includes = append(s.Includes, include)
	return true
}

type Include struct {
	Src  string   `json:"src"`
	Tags []string `json:"tags"`
}

func (i Include) IsSame(include Include) bool {
	return i.Src == include.Src
}

func (i *Include) UnmarshalJSON(bb []byte) error {
	var incStruct struct {
		Src  string   `json:"src"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(bb, &incStruct); err != nil {
		// NOTE: Migration: probably this is an old version of include. This version is just a string.
		var inc string
		if errStr := json.Unmarshal(bb, &inc); errStr != nil {
			return fmt.Errorf("unmarshal Include: %w", errors.Join(err, errStr))
		}

		i.Src = inc
		i.Tags = []string{}
		return nil
	}

	*i = incStruct

	return nil
}

type Stats struct {
	Version        string                          `json:"version"`
	ToolsByWorkdir map[string]map[string]time.Time `json:"tools"`
}

// ToolState describe a state of this tool.
type ToolState struct {
	Module  ModuleInfo
	Tool    Tool
	LastUse optional.Val[time.Time]
}

type RemoteSpec struct {
	Source string   `json:"source"`
	Spec   Spec     `json:"spec"`
	Tags   []string `json:"tags"`
}

func (r *RemoteSpec) UnmarshalJSON(bb []byte) error {
	// NOTE(zhuravlev): Migration: from Tags to tags
	var spec struct {
		Source string   `json:"source"`
		Spec   Spec     `json:"spec"`
		Tags   []string `json:"tags"`
	}
	if err := json.Unmarshal(bb, &spec); err != nil {
		var specOld struct {
			Source string   `json:"Source"`
			Spec   Spec     `json:"Spec"`
			Tags   []string `json:"Tags"`
		}
		if errOld := json.Unmarshal(bb, &specOld); errOld != nil {
			return fmt.Errorf("unmarshal RemoteSpec: %w", errors.Join(err, errOld))
		}

		*r = RemoteSpec(specOld)
		return nil
	}

	*r = spec

	return nil
}

type Lock struct {
	Tools   Tools        `json:"tools"`
	Remotes []RemoteSpec `json:"remotes"`
}

func (l *Lock) FromSpec(spec *Spec) {
	if l.Remotes == nil {
		l.Remotes = make([]RemoteSpec, 0)
	}

	l.Tools = make(Tools, 0, len(spec.Tools))
	for _, tool := range spec.Tools {
		l.Tools.Add(tool)
	}

	// TODO(zhuravlev): should we refresh remotes from spec?

	for _, remote := range l.Remotes {
		for _, tool := range remote.Spec.Tools {
			tool.Tags = append(tool.Tags, remote.Tags...)
			l.Tools.Add(tool)
		}
	}
}
