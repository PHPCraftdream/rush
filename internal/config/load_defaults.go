// Default-value application: setDefaults fills in every option, path, and
// model-map field that the merged config files left unset.
package config

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/fsext"
)

func (c *Config) setDefaults(workingDir, dataDir string) {
	if c.Options == nil {
		c.Options = &Options{}
	}
	if c.Options.TUI == nil {
		c.Options.TUI = &TUIOptions{}
	}
	if len(c.Options.GlobalContextPaths) == 0 {
		crushConfigDir := filepath.Dir(GlobalConfig())
		c.Options.GlobalContextPaths = []string{
			filepath.Join(crushConfigDir, "CRUSH.md"),
			filepath.Join(filepath.Dir(crushConfigDir), "AGENTS.md"),
		}
	}
	slices.Sort(c.Options.GlobalContextPaths)
	c.Options.GlobalContextPaths = slices.Compact(c.Options.GlobalContextPaths)

	if dataDir != "" {
		c.Options.DataDirectory = dataDir
	} else if c.Options.DataDirectory == "" {
		if path, ok := fsext.LookupClosestBounded(workingDir, projectBoundary(workingDir), defaultDataDirectory); ok {
			c.Options.DataDirectory = path
		} else {
			c.Options.DataDirectory = filepath.Join(workingDir, defaultDataDirectory)
		}
	}
	c.Options.DataDirectory = filepath.Clean(filepathext.SmartJoin(workingDir, c.Options.DataDirectory))
	if c.Providers == nil {
		c.Providers = csync.NewMap[string, ProviderConfig]()
	}
	if c.Models == nil {
		c.Models = make(map[SelectedModelType]SelectedModel)
	}
	if c.RecentModels == nil {
		c.RecentModels = make(map[SelectedModelType][]SelectedModel)
	}
	if c.MCP == nil {
		c.MCP = make(map[string]MCPConfig)
	}

	// Add the default context paths if they are not already present
	c.Options.ContextPaths = append(slices.Clone(defaultContextPaths), c.Options.ContextPaths...)
	slices.Sort(c.Options.ContextPaths)
	c.Options.ContextPaths = slices.Compact(c.Options.ContextPaths)

	// Add the default skills directories if not already present.
	for _, dir := range GlobalSkillsDirs() {
		if !slices.Contains(c.Options.SkillsPaths, dir) {
			c.Options.SkillsPaths = append(c.Options.SkillsPaths, dir)
		}
	}

	// Project specific skills dirs.
	c.Options.SkillsPaths = append(c.Options.SkillsPaths, ProjectSkillsDir(workingDir)...)

	if str, ok := os.LookupEnv("CRUSH_DISABLE_PROVIDER_AUTO_UPDATE"); ok {
		c.Options.DisableProviderAutoUpdate, _ = strconv.ParseBool(str)
	}

	if str, ok := os.LookupEnv("CRUSH_DISABLE_DEFAULT_PROVIDERS"); ok {
		c.Options.DisableDefaultProviders, _ = strconv.ParseBool(str)
	}

	if c.Options.Attribution == nil {
		c.Options.Attribution = &Attribution{
			TrailerStyle:  TrailerStyleAssistedBy,
			GeneratedWith: true,
		}
	} else if c.Options.Attribution.TrailerStyle == "" {
		// Migrate deprecated co_authored_by or apply default
		if c.Options.Attribution.CoAuthoredBy != nil {
			if *c.Options.Attribution.CoAuthoredBy {
				c.Options.Attribution.TrailerStyle = TrailerStyleCoAuthoredBy
			} else {
				c.Options.Attribution.TrailerStyle = TrailerStyleNone
			}
		} else {
			c.Options.Attribution.TrailerStyle = TrailerStyleAssistedBy
		}
	}
	c.Options.InitializeAs = cmp.Or(c.Options.InitializeAs, defaultInitializeAs)
}
