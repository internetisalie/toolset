package link

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/afero"

	"github.com/kazhuravlev/toolset/internal/fsh"
)

// ManifestName is the file in the target dir that remembers which entries each project made.
// --prune uses it to tell "made by this project earlier" from "somebody else's".
const ManifestName = ".toolset-link.json"

// Operations reported by Apply.
const (
	OpLink      = "link"
	OpUnchanged = "unchanged"
	OpRemove    = "remove"
	OpSkip      = "skip"
)

// Result is what happened to one entry.
type Result struct {
	Name   string
	Op     string
	Detail string
}

type manifest struct {
	Projects map[string][]string `json:"projects"`
}

type state int

const (
	stateAbsent state = iota
	stateOurs
	stateForeign
)

// Apply creates the entries in dir.
//
// It first checks every entry, and creates nothing when any of them would replace a file that
// toolset did not make. An entry is replaced only when it is a shim carrying the marker, or a
// symlink into the toolset cache dir. With prune, entries this project made earlier for tools that
// are no longer in the plan are removed.
func Apply(fs fsh.FS, dir string, p Params, entries []Entry, prune bool) ([]Result, error) {
	if err := fs.MkdirAll(dir, fsh.DefaultDirPerm); err != nil {
		return nil, fmt.Errorf("create dir (%s): %w", dir, err)
	}

	if err := checkConflicts(fs, dir, p, entries); err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(entries))

	for _, e := range entries {
		res, err := write(fs, dir, p, e)
		if err != nil {
			return results, err
		}

		results = append(results, res)
	}

	man, err := readManifest(fs, dir)
	if err != nil {
		return results, err
	}

	current := make(map[string]bool, len(entries))
	for _, e := range entries {
		current[e.Name] = true
	}

	keep := make(map[string]bool, len(entries))
	for name := range current {
		keep[name] = true
	}

	for _, name := range man.Projects[p.Project] {
		if current[name] {
			continue
		}

		if !prune {
			keep[name] = true // remembered, so a later --prune can still find it
			continue
		}

		res, err := remove(fs, dir, p, name)
		if err != nil {
			return results, err
		}

		if res.Op != "" {
			results = append(results, res)
		}
	}

	if err := writeManifest(fs, dir, man, p.Project, keep); err != nil {
		return results, err
	}

	return results, nil
}

func checkConflicts(fs fsh.FS, dir string, p Params, entries []Entry) error {
	var problems []string

	for _, e := range entries {
		path := filepath.Join(dir, e.Name)

		st, why, err := classify(fs, path, p.CacheDir)
		if err != nil {
			return err
		}

		if st == stateForeign {
			problems = append(problems, fmt.Sprintf("%s: %s", path, why))
		}

		if p.windows() {
			// cmd runs name.com, name.exe and name.bat before name.cmd, so any of them would shadow the shim.
			base := strings.TrimSuffix(e.Name, ".cmd")

			for _, ext := range []string{".com", ".exe", ".bat"} {
				other := filepath.Join(dir, base+ext)
				if fsh.IsExists(fs, other) {
					problems = append(problems, fmt.Sprintf("%s: would run instead of %s", other, path))
				}
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("refusing to replace files that toolset did not create (nothing was changed):\n  %s", strings.Join(problems, "\n  "))
	}

	return nil
}

func write(fs fsh.FS, dir string, p Params, e Entry) (Result, error) {
	path := filepath.Join(dir, e.Name)

	st, _, err := classify(fs, path, p.CacheDir)
	if err != nil {
		return Result{}, err
	}

	if st == stateOurs {
		same, err := isCurrent(fs, path, e)
		if err != nil {
			return Result{}, err
		}

		if same {
			return Result{Name: e.Name, Op: OpUnchanged}, nil
		}

		// Remove first: writing a shim over a symlink would write into the tool's binary.
		if err := fs.Remove(path); err != nil {
			return Result{}, fmt.Errorf("remove (%s): %w", path, err)
		}
	}

	switch e.Kind {
	case KindSymlink:
		if err := fs.SymlinkIfPossible(e.Target, path); err != nil {
			return Result{}, fmt.Errorf("symlink %s to %s: %w", path, e.Target, err)
		}

		return Result{Name: e.Name, Op: OpLink, Detail: e.Target}, nil
	default:
		if err := afero.WriteFile(fs, path, []byte(e.Content), 0o755); err != nil {
			return Result{}, fmt.Errorf("write (%s): %w", path, err)
		}

		return Result{Name: e.Name, Op: OpLink, Detail: "shim"}, nil
	}
}

func isCurrent(fs fsh.FS, path string, e Entry) (bool, error) {
	if e.Kind == KindSymlink {
		target, err := readlink(fs, path)
		if err != nil {
			return false, nil //nolint:nilerr // not a symlink (or unreadable): replace it
		}

		return target == e.Target, nil
	}

	bb, err := afero.ReadFile(fs, path)
	if err != nil {
		return false, fmt.Errorf("read (%s): %w", path, err)
	}

	return string(bb) == e.Content, nil
}

func remove(fs fsh.FS, dir string, p Params, name string) (Result, error) {
	path := filepath.Join(dir, name)

	st, why, err := classify(fs, path, p.CacheDir)
	if err != nil {
		return Result{}, err
	}

	switch st {
	case stateAbsent:
		return Result{}, nil
	case stateForeign:
		return Result{Name: name, Op: OpSkip, Detail: why}, nil
	}

	if err := fs.Remove(path); err != nil {
		return Result{}, fmt.Errorf("remove (%s): %w", path, err)
	}

	return Result{Name: name, Op: OpRemove}, nil
}

// classify says who made the file at path. The second result explains a foreign one.
func classify(fs fsh.FS, path, cacheDir string) (state, string, error) {
	fi, err := lstat(fs, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stateAbsent, "", nil
		}

		return 0, "", fmt.Errorf("stat (%s): %w", path, err)
	}

	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := readlink(fs, path)
		if err != nil {
			return 0, "", fmt.Errorf("read symlink (%s): %w", path, err)
		}

		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}

		if within(cacheDir, target) {
			return stateOurs, "", nil
		}

		return stateForeign, "a symlink to " + target + ", which is not in the toolset cache", nil
	case fi.IsDir():
		return stateForeign, "a directory", nil
	}

	f, err := fs.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open (%s): %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	head, err := io.ReadAll(io.LimitReader(f, 512))
	if err != nil {
		return 0, "", fmt.Errorf("read (%s): %w", path, err)
	}

	if strings.Contains(string(head), Marker) {
		return stateOurs, "", nil
	}

	return stateForeign, "an existing file", nil
}

func within(root, p string) bool {
	if root == "" {
		return false
	}

	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func lstat(fs fsh.FS, path string) (os.FileInfo, error) {
	if l, ok := fs.(afero.Lstater); ok {
		fi, _, err := l.LstatIfPossible(path)

		return fi, err
	}

	return fs.Stat(path)
}

func readlink(fs fsh.FS, path string) (string, error) {
	l, ok := fs.(afero.LinkReader)
	if !ok {
		return "", errors.New("this file system cannot read symlinks")
	}

	return l.ReadlinkIfPossible(path)
}

func readManifest(fs fsh.FS, dir string) (manifest, error) {
	m := manifest{Projects: map[string][]string{}}

	bb, err := afero.ReadFile(fs, filepath.Join(dir, ManifestName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, nil
		}

		return m, fmt.Errorf("read %s: %w", ManifestName, err)
	}

	if err := json.Unmarshal(bb, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", ManifestName, err)
	}

	if m.Projects == nil {
		m.Projects = map[string][]string{}
	}

	return m, nil
}

func writeManifest(fs fsh.FS, dir string, m manifest, project string, names map[string]bool) error {
	list := make([]string, 0, len(names))
	for name := range names {
		list = append(list, name)
	}

	sort.Strings(list)

	if len(list) == 0 {
		delete(m.Projects, project)
	} else {
		m.Projects[project] = list
	}

	path := filepath.Join(dir, ManifestName)

	if len(m.Projects) == 0 {
		if fsh.IsExists(fs, path) {
			return fs.Remove(path)
		}

		return nil
	}

	bb, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", ManifestName, err)
	}

	if err := afero.WriteFile(fs, path, append(bb, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", ManifestName, err)
	}

	return nil
}
