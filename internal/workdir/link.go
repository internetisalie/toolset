package workdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/kazhuravlev/toolset/internal/fsh"
	"github.com/kazhuravlev/toolset/internal/link"
	"github.com/kazhuravlev/toolset/internal/workdir/structs"
)

// toLinkTools picks what to link from the tools of the lock. A tool that is not installed is an error.
func toLinkTools(tools []structs.ToolState) ([]link.Tool, error) {
	var missing []string

	in := make([]link.Tool, 0, len(tools))

	for _, ts := range tools {
		if !ts.Module.IsInstalled {
			missing = append(missing, ts.Tool.Module)
			continue
		}

		alias := ts.Tool.Alias.ValDefault("")

		name := alias
		if name == "" {
			name = ts.Module.Name
		}

		in = append(in, link.Tool{Name: name, Binary: ts.Module.BinPath, Alias: alias})
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("tools are not installed, run `toolset sync` first: %s", strings.Join(missing, ", "))
	}

	return in, nil
}

// Link makes the installed tools of this project callable by name from dir.
// Tools that are not installed are an error: a link to them would dangle.
func (c *Workdir) Link(ctx context.Context, dir string, prune bool) ([]link.Result, error) {
	tools, err := c.GetTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("get tools: %w", err)
	}

	in, err := toLinkTools(tools)
	if err != nil {
		return nil, err
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find the toolset executable: %w", err)
	}

	params := link.Params{
		GOOS:     runtime.GOOS,
		Toolset:  exe,
		Project:  c.locations.ProjectRootDir,
		CacheDir: c.locations.CacheDir,
	}

	entries, err := link.Plan(params, in)
	if err != nil {
		return nil, fmt.Errorf("plan links: %w", err)
	}

	absDir, err := fsh.Abs(c.fs, dir)
	if err != nil {
		return nil, fmt.Errorf("abs dir: %w", err)
	}

	if absDir == "" {
		return nil, errors.New("directory is required")
	}

	results, err := link.Apply(c.fs, absDir, params, entries, prune)
	if err != nil {
		return results, fmt.Errorf("link into %s: %w", absDir, err)
	}

	return results, nil
}
