package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kazhuravlev/toolset/internal/humanize"
	"github.com/kazhuravlev/toolset/internal/timeh"
	"github.com/kazhuravlev/toolset/internal/toolversion"

	"github.com/kazhuravlev/optional"
	"github.com/kazhuravlev/toolset/internal/fsh"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/kazhuravlev/toolset/internal/workdir"
	"github.com/kazhuravlev/toolset/internal/workdir/structs"
	cli "github.com/urfave/cli/v2"
)

const (
	keyParallel = "parallel"
	keyCopyFrom = "copy-from"
	keyInclude  = "include"
	keyTags     = "tags"
	keyUnused   = "unused"
	keyPin      = "pin"
	keyPrune    = "prune"
	keyDir      = "dir"

	keyDockerEntrypoint = "entrypoint"
	keyDockerArg        = "arg"
	keyDockerVolume     = "volume"
	keyDockerWorkdir    = "workdir"
	keyDockerNetwork    = "network"
	keyDockerEnv        = "env"
)

// flagsDocker configure the docker runtime. They are ignored by other runtimes (and rejected when set).
var flagsDocker = []cli.Flag{
	&cli.StringFlag{Name: keyDockerEntrypoint, Usage: "docker: override the image entrypoint"},
	&cli.StringSliceFlag{Name: keyDockerArg, Usage: "docker: fixed argument placed before user arguments (repeatable)"},
	&cli.StringSliceFlag{Name: keyDockerVolume, Usage: "docker: volume src:dst[:ro|rw]; $PROJECT, $PWD, $HOME are expanded at run time (repeatable). Default $PROJECT:/work"},
	&cli.StringFlag{Name: keyDockerWorkdir, Usage: "docker: working directory inside the container"},
	&cli.StringFlag{Name: keyDockerNetwork, Usage: "docker: none (default), bridge, host or a network name"},
	&cli.StringSliceFlag{Name: keyDockerEnv, Usage: "docker: host env variable name to pass through (repeatable)"},
}

func dockerConfigFromFlags(c *cli.Context) optional.Val[structs.DockerConfig] {
	cfg := structs.DockerConfig{
		Entrypoint: c.String(keyDockerEntrypoint),
		Args:       c.StringSlice(keyDockerArg),
		Volumes:    c.StringSlice(keyDockerVolume),
		Workdir:    c.String(keyDockerWorkdir),
		Network:    c.String(keyDockerNetwork),
		Env:        c.StringSlice(keyDockerEnv),
	}

	if cfg.Entrypoint == "" && len(cfg.Args) == 0 && len(cfg.Volumes) == 0 &&
		cfg.Workdir == "" && cfg.Network == "" && len(cfg.Env) == 0 {
		return optional.Empty[structs.DockerConfig]()
	}

	return optional.New(cfg)
}

var flagParallel = &cli.IntFlag{
	Name:    keyParallel,
	Aliases: []string{"p"},
	Usage:   "Max parallel workers",
	Value:   4,
}

var flagPin = &cli.BoolFlag{
	Name:  keyPin,
	Usage: "pin binary digest / commit hash in the lock file for supply-chain verification",
}

func main() {
	app := &cli.App{
		Name:  "toolset",
		Usage: "Manage local toolsets",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  keyDir,
				Usage: "directory to look for the toolset spec from (default: the current directory). Put it before the command",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "version",
				Usage: "show toolset version",
				Description: `Display the current version of toolset.

	$ toolset version`,
				Action: func(c *cli.Context) error {
					fmt.Println("version:", toolversion.GetVersion())
					return nil
				},
			},
			{
				Name:  "init",
				Usage: "init toolset in specified directory",
				Description: `Initialize a new toolset configuration in the specified directory.
Creates .toolset.json and .toolset.lock.json files to start managing tools.

	$ toolset init
	$ toolset init ./myproject

Optionally copy tools from an existing toolset:

	$ toolset init --copy-from=../other-project/.toolset.json`,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     keyCopyFrom,
						Usage:    "specify addr to source file that will be copied into new config",
						Required: false,
					},
				},
				Action: cmdInit,
				Args:   true,
			},
			{
				Name:  "add",
				Usage: "add tool to .toolset.json",
				Description: `Add tools to local configuration to fix the using version.

	$ toolset add <RUNTIME> <TOOL>
	$ toolset add go 				github.com/golangci/golangci-lint/cmd/golangci-lint@v1.61.0

At this point tool will not be installed. In order to install added tool please run

	$ toolset sync`,
				Action: withWorkdir(cmdAdd),
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     keyCopyFrom,
						Usage:    "specify addr to source file that will be copied into current config",
						Required: false,
					},
					&cli.StringFlag{
						Name:     keyInclude,
						Usage:    "specify addr to source file that will be included into current config",
						Required: false,
					},
					&cli.StringSliceFlag{
						Name:     keyTags,
						Usage:    "add one or more tags to this tool",
						Required: false,
					},
					flagPin,
				},
				Args: true,
			},
			{
				Name:  "sync",
				Usage: "install all required tools from toolset file",
				Description: `Install or update all tools defined in .toolset.json to match the configuration.
This command downloads, builds, and installs tools that are missing or outdated.
Updates .toolset.lock.json with installed versions.

	$ toolset sync
	$ toolset sync --parallel=8
	$ toolset sync --tags=linters,formatters

When cache is configured, sync automatically downloads from cache and uploads new builds.`,
				Action: withWorkdir(cmdSync),
				Flags: []cli.Flag{
					flagParallel,
					&cli.StringSliceFlag{
						Name:     keyTags,
						Usage:    "filter tools by tags",
						Required: false,
					},
				},
			},
			{
				Name:  "run",
				Usage: "run installed tool by its name",
				Description: `Execute an installed tool with the specified arguments.
The tool must be added and synced before running.

	$ toolset run golangci-lint --version
	$ toolset run gofumpt -l -w .
	$ toolset run <tool-name> [args...]

If tool is not added, run 'toolset add'. If not installed, run 'toolset sync'.`,
				Action: withWorkdir(cmdRun),
				Args:   true,
			},
			{
				Name:  "upgrade",
				Usage: "upgrade deps to the latest versions",
				Description: `Upgrade tools to their latest available versions and sync them.
Updates .toolset.json with new versions and installs them.

	$ toolset upgrade
	$ toolset upgrade golangci-lint
	$ toolset upgrade --tags=linters
	$ toolset upgrade --parallel=8

Upgrades all tools by default. Specify a module name or use --tags to filter.`,
				Action: withWorkdir(cmdUpgrade),
				Flags: []cli.Flag{
					flagParallel,
					&cli.StringSliceFlag{
						Name:     keyTags,
						Usage:    "filter tools by tags",
						Required: false,
					},
					flagPin,
				},
				Args: true,
			},
			{
				Name:  "ensure",
				Usage: "ensure concrete version is exists. work like upsert semantic",
				Description: `Ensure a specific tool version exists in the configuration.
Works like upsert: adds the tool if missing, or updates if present with different version.

	$ toolset ensure go github.com/golangci/golangci-lint/cmd/golangci-lint@v1.61.0
	$ toolset ensure go github.com/golangci/golangci-lint/cmd/golangci-lint@v1.61.0 golangci
	$ toolset ensure go <module@version> [alias] --tags=linters

This does NOT install the tool. Run 'toolset sync' afterward to install.`,
				Action: withWorkdir(cmdEnsureModuleVersion),
				Flags: []cli.Flag{
					&cli.StringSliceFlag{
						Name:     keyTags,
						Usage:    "filter tools by tags",
						Required: false,
					},
					flagPin,
				},
				Args: true,
			},
			{
				Name:  "list",
				Usage: "list of project tools and their stats",
				Description: `Display a table of all tools with runtime, version, installation status, and usage statistics.
Shows tool metadata including aliases, tags, and last usage time.

	$ toolset list
	$ toolset list --unused

Use --unused flag to find tools that have never been executed (helpful for cleanup).`,
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  keyUnused,
						Usage: "list tools that are unused. Useful when you want to check which tools can be deleted (or excluded from tag)",
						Value: false,
					},
				},
				Action: withWorkdir(cmdList),
			},
			{
				Name:  "link",
				Usage: "make installed tools callable by name from a directory",
				Description: `Create an entry for every installed tool in the given directory, so the tools can be
called by name when that directory is on PATH (a CI bin dir, ~/bin, ...).

	$ toolset link --prune ~/bin
	$ toolset link .tools/bin

Tools with a binary are symlinked (on Windows: a .cmd file that calls it). Tools without one, such
as docker images, get a small script that calls 'toolset run' for this project, so it works from
any directory.

An existing file is replaced only if toolset made it. If anything else is in the way, nothing is
changed. Tools that are not installed are an error: run 'toolset sync' first.

--prune also removes entries this project made earlier for tools that are no longer in the spec.
Flags must come before the directory.`,
				Action: withWorkdir(cmdLink),
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  keyPrune,
						Usage: "remove entries made earlier by this project for tools that are no longer in the spec",
					},
				},
				Args: true,
			},
			{
				Name:  "which",
				Usage: "show path to the actual binary",
				Description: `Display the full filesystem path to the installed tool binary.
Useful for debugging or integrating with other tools.

	$ toolset which golangci-lint
	$ toolset which gofumpt goimports

Can query multiple tools at once.`,
				Action: withWorkdir(cmdWhich),
				Args:   true,
			},
			{
				Name:  "remove",
				Usage: "remove tool",
				Description: `Remove one or more tools from the toolset configuration.
Removes from .toolset.json but does not delete the installed binary.

	$ toolset remove golangci-lint
	$ toolset remove gofumpt goimports

Can remove multiple tools in a single command.`,
				Action: withWorkdir(cmdRemove),
				Args:   true,
			},
			{
				Name:  "info",
				Usage: "show information and stats",
				Description: `Display system information including toolset version, cache size, and file locations.
Shows paths to configuration files, cache directory, and environment variables.

	$ toolset info

Useful for debugging and understanding the toolset environment.`,
				Action: withWorkdir(cmdInfo),
				Args:   false,
			},
			{
				Name:  "clear-cache",
				Usage: "clear all cache dir and stats",
				Description: `Delete the entire cache directory including all installed tools and statistics.
This will free up disk space but require re-building tools on next sync.

	$ toolset clear-cache

WARNING: This removes all cached binaries. Tools will need to be rebuilt.`,
				Action: withWorkdir(cmdClearCache),
				Args:   false,
			},
			{
				Name:  "runtime",
				Usage: "manage runtimes",
				Description: `Manage runtime environments for executing tools.
Runtimes are language-specific environments (e.g., Go versions) used to run tools.

	$ toolset runtime add go@1.22
	$ toolset runtime list`,
				Subcommands: []*cli.Command{
					{
						Name:  "add",
						Usage: "add new runtime",
						Description: `Install a specific runtime version in the local project directory.
Runtimes are needed to execute tools built for that runtime.

	$ toolset runtime add go@1.22
	$ toolset runtime add go@1.23.4

Downloads and installs the runtime if not already present.`,
						Action: withWorkdir(cmdRuntimeAdd),
						Args:   true,
					},
					{
						Name:  "list",
						Usage: "list all runtimes",
						Description: `Display all runtimes currently installed in the project.
Shows runtime names and versions available for use.

	$ toolset runtime list`,
						Action: withWorkdir(cmdRuntimeList),
					},
				},
			},
		},
	}

	for _, cmd := range app.Commands {
		if cmd.Name == "add" || cmd.Name == "ensure" {
			cmd.Flags = append(cmd.Flags, flagsDocker...)
		}
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func cmdInit(c *cli.Context) error {
	ctx := c.Context

	fs := fsh.NewRealFS()

	targetDir := c.Args().First()
	if targetDir == "" && c.IsSet(keyDir) {
		targetDir = c.String(keyDir)
	}

	if targetDir == "" {
		targetDir = "."
	}

	if err := workdir.Init(ctx, fs, targetDir); err != nil {
		return fmt.Errorf("init workdir: %w", err)
	}

	fmt.Println("Spec created")

	if val := c.String(keyCopyFrom); val != "" {
		wd, err := workdir.New(ctx, fs, targetDir)
		if err != nil {
			return fmt.Errorf("new workdir: %w", err)
		}

		count, err := wd.CopySource(ctx, val, nil)
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}

		if err := wd.Save(ctx); err != nil {
			return fmt.Errorf("save workdir: %w", err)
		}

		fmt.Println("Copied tools:", count)
	}

	return nil
}

func withWorkdir(fn func(c *cli.Context, wd *workdir.Workdir) error) func(c *cli.Context) error {
	return func(c *cli.Context) error {
		fs := fsh.NewRealFS()

		dir := c.String(keyDir)
		if dir == "" {
			dir = "./"
		}

		wd, err := workdir.New(c.Context, fs, dir)
		if err != nil {
			return fmt.Errorf("new workdir: %w", err)
		}

		return fn(c, wd)
	}
}

func cmdAdd(c *cli.Context, wd *workdir.Workdir) error {
	ctx := c.Context

	tags := c.StringSlice(keyTags)

	if val := c.String(keyCopyFrom); val != "" {
		count, err := wd.CopySource(ctx, val, tags)
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}

		if err := wd.Save(ctx); err != nil {
			return fmt.Errorf("save workdir: %w", err)
		}

		fmt.Println("Copied tools:", count)

		return nil
	}

	if val := c.String(keyInclude); val != "" {
		count, err := wd.AddInclude(ctx, val, tags)
		if err != nil {
			return fmt.Errorf("include: %w", err)
		}

		if err := wd.Save(ctx); err != nil {
			return fmt.Errorf("save workdir: %w", err)
		}

		fmt.Println("Included tools:", count)

		return nil
	}

	runtime := c.Args().First()
	module := c.Args().Get(1)

	var alias optional.Val[string]
	if c.Args().Len() == 3 {
		aliasStr := c.Args().Get(2)
		alias.Set(aliasStr)
	}

	pin := c.Bool(keyPin)

	wasAdded, mod, err := wd.Add(ctx, runtime, module, alias, tags, pin, dockerConfigFromFlags(c))
	if err != nil {
		return fmt.Errorf("add module: %w", err)
	}

	if err := wd.Save(ctx); err != nil {
		return fmt.Errorf("save context: %w", err)
	}

	if !wasAdded {
		fmt.Printf("tool already exists in toolset: %s\n", mod)
	} else {
		fmt.Printf("tool added to toolset: %s\n", mod)
	}

	return nil
}

func cmdRuntimeAdd(c *cli.Context, wd *workdir.Workdir) error {
	ctx := c.Context

	runtime := c.Args().First()

	if err := wd.RuntimeAdd(ctx, runtime); err != nil {
		return fmt.Errorf("add runtime: %w", err)
	}

	if err := wd.Save(ctx); err != nil {
		return fmt.Errorf("save context: %w", err)
	}

	return nil
}

func cmdRuntimeList(_ *cli.Context, wd *workdir.Workdir) error {
	t := table.NewWriter()
	t.AppendHeader(table.Row{
		"Runtime",
	})

	list := wd.RuntimeList()

	rows := make([]table.Row, 0, len(list))
	for _, name := range list {
		rows = append(rows, table.Row{name})
	}

	t.AppendRows(rows)

	res := t.Render()
	fmt.Println(res)

	return nil
}

func cmdRun(c *cli.Context, wd *workdir.Workdir) error {
	ctx := c.Context

	target := c.Args().First()
	if target == "" {
		return fmt.Errorf("target is required")
	}

	if err := wd.RunTool(ctx, target, c.Args().Tail()...); err != nil {
		if errors.Is(err, workdir.ErrToolNotFoundInSpec) {
			fmt.Println("tool not added. Run `toolset add --help` to add this tool")
			os.Exit(1)
			return nil
		}

		if errors.Is(err, workdir.ErrToolNotInstalled) {
			fmt.Println("tool not installed. Run `toolset sync --help` to install tool before run")
			os.Exit(1)
			return nil
		}

		var errRun structs.RunError
		if errors.As(err, &errRun) {
			os.Exit(errRun.ExitCode)
			return nil
		}

		return fmt.Errorf("run tool: %w", err)
	}

	return nil
}

func cmdSync(c *cli.Context, wd *workdir.Workdir) error {
	ctx := c.Context

	maxWorkers := c.Int(keyParallel)
	tags := c.StringSlice(keyTags)

	if err := wd.Sync(ctx, maxWorkers, tags); err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	if err := wd.Save(ctx); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	return nil
}

func cmdUpgrade(c *cli.Context, wd *workdir.Workdir) error {
	ctx := c.Context

	maxWorkers := c.Int(keyParallel)
	tags := c.StringSlice(keyTags)
	module := c.Args().First()
	pin := c.Bool(keyPin)

	if module != "" && len(tags) != 0 {
		return fmt.Errorf("can't use both module and tags")
	}

	filter := func(tool structs.Tool) bool { return true }
	if module != "" {
		fmt.Println("upgrade module:", module)
		filter = func(tool structs.Tool) bool {
			if alias, ok := tool.Alias.Get(); ok {
				if alias == module {
					return true
				}
			}

			moduleName := tool.ModuleName()

			return moduleName == module || strings.HasSuffix(moduleName, "/"+module)
		}
	}

	if len(tags) != 0 {
		fmt.Println("upgrade modules by tags:", tags)
		filter = func(tool structs.Tool) bool {
			for _, tag := range tool.Tags {
				for _, tag2 := range tags {
					if tag == tag2 {
						return true
					}
				}
			}

			return false
		}
	}

	if err := wd.Upgrade(ctx, filter, pin); err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}

	if err := wd.Save(ctx); err != nil {
		return fmt.Errorf("save context: %w", err)
	}

	if err := wd.Sync(ctx, maxWorkers, tags); err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	if err := wd.Save(ctx); err != nil {
		return fmt.Errorf("save context: %w", err)
	}

	return nil
}

func cmdList(c *cli.Context, wd *workdir.Workdir) error {
	ctx := c.Context

	onlyUnused := c.Bool(keyUnused)

	tools, err := wd.GetTools(ctx)
	if err != nil {
		return fmt.Errorf("get tools: %w", err)
	}

	if onlyUnused {
		tools2 := make([]structs.ToolState, 0, len(tools))
		for _, tool := range tools {
			if tool.LastUse.HasVal() {
				continue
			}

			tools2 = append(tools2, tool)
		}

		tools = tools2
	}

	sort.SliceStable(tools, func(i, j int) bool {
		return tools[i].Tool.ModuleName() < tools[j].Tool.ModuleName()
	})

	rows := make([]table.Row, 0, len(tools))
	for _, ts := range tools {
		lastUse := "---"
		if val, ok := ts.LastUse.Get(); ok {
			lastUse = timeh.Duration(time.Since(val))
		}

		rows = append(rows, table.Row{
			ts.Tool.Runtime,
			ts.Module.Name,
			ts.Module.Mod.Version(),
			ts.Module.IsInstalled,
			lastUse,
			ts.Module.IsPrivate,
			ts.Tool.Alias.ValDefault("---"),
			strings.Join(ts.Tool.Tags, ","),
			ts.Tool.Module,
		})
	}

	t := table.NewWriter()
	t.AppendHeader(table.Row{
		"Runtime",
		"Name",
		"Version",
		"Installed",
		"Last Usage",
		"Private",
		"Alias",
		"Tags",
		"Module",
	})

	t.AppendRows(rows)

	res := t.Render()
	fmt.Println(res)

	return nil
}

func cmdWhich(c *cli.Context, wd *workdir.Workdir) error {
	targets := c.Args().Slice()
	if len(targets) == 0 {
		return fmt.Errorf("target is required")
	}

	for _, target := range targets {
		ts, err := wd.FindTool(target)
		if err != nil {
			if errors.Is(err, workdir.ErrToolNotFoundInSpec) {
				fmt.Println("tool not added. Run `toolset add --help` to add tool")
				os.Exit(1)
				return nil
			}

			if errors.Is(err, workdir.ErrToolNotInstalled) {
				fmt.Println("tool not installed. Run `toolset sync --help` to install tool before run")
				os.Exit(1)
				return nil
			}

			return fmt.Errorf("find tool: %w", err)
		}

		if ts.Module.BinPath == "" {
			// No binary on disk (docker): show what is run instead.
			fmt.Println(ts.Tool.Runtime + ":" + ts.Tool.Module)
			continue
		}

		fmt.Println(ts.Module.BinPath)
	}

	return nil
}

func cmdLink(c *cli.Context, wd *workdir.Workdir) error {
	if c.NArg() != 1 {
		return fmt.Errorf("exactly one directory is required (flags go before it)")
	}

	results, err := wd.Link(c.Context, c.Args().First(), c.Bool(keyPrune))

	for _, res := range results {
		switch res.Detail {
		case "":
			fmt.Println(res.Op+":", res.Name)
		default:
			fmt.Println(res.Op+":", res.Name, "("+res.Detail+")")
		}
	}

	if err != nil {
		return fmt.Errorf("link: %w", err)
	}

	return nil
}

func cmdRemove(c *cli.Context, wd *workdir.Workdir) error {
	ctx := c.Context

	targets := c.Args().Slice()
	if len(targets) == 0 {
		return fmt.Errorf("target is required")
	}

	for _, target := range targets {
		if err := wd.RemoveTool(ctx, target); err != nil {
			return fmt.Errorf("remove tool (%s): %w", target, err)
		}
	}

	if err := wd.Save(ctx); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	return nil
}

func cmdInfo(_ *cli.Context, wd *workdir.Workdir) error {
	info, err := wd.GetSystemInfo()
	if err != nil {
		return fmt.Errorf("get system info: %w", err)
	}

	t := table.NewWriter()
	t.AppendHeader(table.Row{
		"Property",
		"Value",
	})

	rows := []table.Row{
		{"Version:", toolversion.GetVersion()},
		{"Cache Size:", humanize.Bytes(info.Storage.TotalBytes)},
		{"Cache dir:", info.Locations.CacheDir},
		{"Toolset File:", info.Locations.ToolsetFile},
		{"Toolset Lock File:", info.Locations.ToolsetLockFile},
		{"Project Root Dir:", info.Locations.ProjectRootDir},
		{"Current Dir:", info.Locations.CurrentDir},
		{"Stats File:", info.Locations.StatsFile},
	}

	for _, env := range info.Envs {
		rows = append(rows, table.Row{"ENV:" + env[0], env[1]})
	}

	t.AppendRows(rows)

	res := t.Render()
	fmt.Println(res)

	return nil
}

func cmdClearCache(_ *cli.Context, wd *workdir.Workdir) error {
	info, err := wd.GetSystemInfo()
	if err != nil {
		return fmt.Errorf("get system info: %w", err)
	}

	fmt.Println("Removing cache dir:", info.Locations.CacheDir)

	if err := os.RemoveAll(info.Locations.CacheDir); err != nil {
		return fmt.Errorf("remove cache dir: %w", err)
	}

	fmt.Println("Done!")

	return nil
}

func cmdEnsureModuleVersion(c *cli.Context, wd *workdir.Workdir) error {
	ctx := c.Context

	tags := c.StringSlice(keyTags)
	runtime := c.Args().First()
	module := c.Args().Get(1)

	if runtime == "" || module == "" {
		return fmt.Errorf("runtime and module is required")
	}

	var alias optional.Val[string]
	if c.Args().Len() == 3 {
		aliasStr := c.Args().Get(2)
		alias.Set(aliasStr)
	}

	pin := c.Bool(keyPin)

	mod, err := wd.Ensure(ctx, runtime, module, alias, tags, pin, dockerConfigFromFlags(c))
	if err != nil {
		return fmt.Errorf("ensure module: %w", err)
	}

	fmt.Println("Module added:", runtime, mod)

	if err := wd.Save(ctx); err != nil {
		return fmt.Errorf("save workdir: %w", err)
	}

	return nil
}
