// Package link puts installed tools into a directory, so they can be called by name.
//
// A tool with a binary is linked with a symlink (a .cmd shim on Windows, where symlinks need a
// privilege most users lack). A tool without one (docker) gets a shim that calls `toolset run`.
package link

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Marker is written into every shim. It is how a shim is recognized as ours.
const Marker = "toolset-link"

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)

// Tool is an installed tool to link.
type Tool struct {
	// Name is the entry name: the alias, or the name of the binary.
	Name string
	// Binary is the absolute path of the installed binary. Empty when there is none (docker).
	Binary string
	// Alias is the name `toolset run` takes. Required when Binary is empty.
	Alias string
}

// Params describe the environment the entries are made for.
type Params struct {
	// GOOS selects the flavour of the entries: "windows" gives .cmd shims, anything else symlinks and sh shims.
	GOOS string
	// Toolset is the absolute path of the toolset executable, called by shims of tools without a binary.
	Toolset string
	// Project is the project root; shims pass it as --dir, so they work from any directory.
	Project string
	// CacheDir is where toolset installs tools. A symlink into it is considered to be made by toolset.
	CacheDir string
}

func (p Params) windows() bool { return p.GOOS == "windows" }

// Kind is the way an entry is made.
type Kind int

const (
	KindSymlink Kind = iota
	KindShell
	KindCmd
)

// Entry is a file to create in the target directory.
type Entry struct {
	// Name is the file name, with the extension on Windows.
	Name string
	Kind Kind
	// Target is what a symlink points to.
	Target string
	// Content is the text of a shim.
	Content string
}

// Plan makes the entries for the tools. It touches nothing.
func Plan(p Params, tools []Tool) ([]Entry, error) {
	windows := p.windows()
	seen := make(map[string]string, len(tools))
	entries := make([]Entry, 0, len(tools))

	for _, t := range tools {
		name := t.Name
		if windows && strings.EqualFold(filepath.Ext(name), ".exe") {
			name = name[:len(name)-len(".exe")]
		}

		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("tool %q: cannot be linked, the name must match %s", t.Name, nameRe)
		}

		if t.Binary == "" {
			// The alias is written into a shim as it is, so it is held to the same rule as a file name.
			if !nameRe.MatchString(t.Alias) {
				return nil, fmt.Errorf("tool %q: has no binary, and %q is not usable as the name to run it by", t.Name, t.Alias)
			}
		}

		entry, err := makeEntry(p, name, t)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", t.Name, err)
		}

		key := entry.Name
		if windows {
			key = strings.ToLower(key)
		}

		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("tools %q and %q both want the name %q", prev, t.Name, entry.Name)
		}

		seen[key] = t.Name

		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	return entries, nil
}

func makeEntry(p Params, name string, t Tool) (Entry, error) {
	if p.windows() {
		content, err := cmdShim(p, t)
		if err != nil {
			return Entry{}, err
		}

		return Entry{Name: name + ".cmd", Kind: KindCmd, Content: content}, nil
	}

	if t.Binary != "" {
		return Entry{Name: name, Kind: KindSymlink, Target: t.Binary}, nil
	}

	return Entry{Name: name, Kind: KindShell, Content: shShim(p, t)}, nil
}

// shShim runs the tool through `toolset run`, pinned to the project so it works from any directory.
func shShim(p Params, t Tool) string {
	return "#!/bin/sh\n" +
		"# " + Marker + " project=" + oneLine(p.Project) + "\n" +
		"exec " + shQuote(p.Toolset) + " --dir " + shQuote(p.Project) + " run " + shQuote(t.Alias) + " \"$@\"\n"
}

func cmdShim(p Params, t Tool) (string, error) {
	var call string

	if t.Binary != "" {
		quoted, err := cmdQuote(t.Binary)
		if err != nil {
			return "", err
		}

		call = quoted + " %*"
	} else {
		exe, err := cmdQuote(p.Toolset)
		if err != nil {
			return "", err
		}

		project, err := cmdQuote(p.Project)
		if err != nil {
			return "", err
		}

		call = exe + " --dir " + project + " run " + t.Alias + " %*"
	}

	return "@echo off\r\n" +
		"REM " + Marker + " project=" + oneLine(p.Project) + "\r\n" +
		call + "\r\n" +
		"exit /b %ERRORLEVEL%\r\n", nil
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdQuote quotes a path for a batch file. A double quote cannot be escaped in a path, and a line
// break would end the command, so those are refused.
func cmdQuote(s string) (string, error) {
	if strings.ContainsAny(s, "\"\r\n") {
		return "", fmt.Errorf("path %q has a character that cannot be used in a .cmd file", s)
	}

	return `"` + strings.ReplaceAll(s, "%", "%%") + `"`, nil
}

// oneLine keeps a value from breaking out of the comment line it is written into.
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
