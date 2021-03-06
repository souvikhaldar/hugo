// Copyright 2019 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package modules

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cast"

	"github.com/gohugoio/hugo/common/maps"

	"github.com/gohugoio/hugo/common/hugo"
	"github.com/gohugoio/hugo/parser/metadecoders"

	"github.com/gohugoio/hugo/hugofs/files"

	"github.com/rogpeppe/go-internal/module"

	"github.com/pkg/errors"

	"github.com/gohugoio/hugo/config"
	"github.com/spf13/afero"
)

var ErrNotExist = errors.New("module does not exist")

const vendorModulesFilename = "modules.txt"

// IsNotExist returns whether an error means that a module could not be found.
func IsNotExist(err error) bool {
	return errors.Cause(err) == ErrNotExist
}

// CreateProjectModule creates modules from the given config.
// This is used in tests only.
func CreateProjectModule(cfg config.Provider) (Module, error) {
	workingDir := cfg.GetString("workingDir")
	var modConfig Config

	mod := createProjectModule(nil, workingDir, modConfig)
	if err := ApplyProjectConfigDefaults(cfg, mod); err != nil {
		return nil, err
	}

	return mod, nil
}

func (h *Client) Collect() (ModulesConfig, error) {
	mc, coll := h.collect(true)
	return mc, coll.err

}

func (h *Client) collect(tidy bool) (ModulesConfig, *collector) {
	c := &collector{
		Client: h,
	}

	c.collect()
	if c.err != nil {
		return ModulesConfig{}, c
	}

	if !c.skipTidy && tidy {
		if err := h.tidy(c.modules, true); err != nil {
			c.err = err
			return ModulesConfig{}, c
		}
	}

	// TODO(bep) consider --ignoreVendor vs removing from go.mod
	var activeMods Modules
	for _, mod := range c.modules {
		if !mod.Config().HugoVersion.IsValid() {
			h.logger.WARN.Printf(`Module %q is not compatible with this Hugo version; run "hugo mod graph" for more information.`, mod.Path())
		}
		if !mod.Disabled() {
			activeMods = append(activeMods, mod)
		}
	}

	return ModulesConfig{
		AllModules:        c.modules,
		ActiveModules:     activeMods,
		GoModulesFilename: c.GoModulesFilename,
	}, c

}

type ModulesConfig struct {
	// All modules, including any disabled.
	AllModules Modules

	// All active modules.
	ActiveModules Modules

	// Set if this is a Go modules enabled project.
	GoModulesFilename string
}

type collected struct {
	// Pick the first and prevent circular loops.
	seen map[string]bool

	// Maps module path to a _vendor dir. These values are fetched from
	// _vendor/modules.txt, and the first (top-most) will win.
	vendored map[string]vendoredModule

	// Set if a Go modules enabled project.
	gomods goModules

	// Ordered list of collected modules, including Go Modules and theme
	// components stored below /themes.
	modules Modules
}

// Collects and creates a module tree.
type collector struct {
	*Client

	// Store away any non-fatal error and return at the end.
	err error

	// Set to disable any Tidy operation in the end.
	skipTidy bool

	*collected
}

func (c *collector) initModules() error {
	c.collected = &collected{
		seen:     make(map[string]bool),
		vendored: make(map[string]vendoredModule),
	}

	// We may fail later if we don't find the mods.
	return c.loadModules()
}

func (c *collector) isSeen(path string) bool {
	key := pathKey(path)
	if c.seen[key] {
		return true
	}
	c.seen[key] = true
	return false
}

func (c *collector) getVendoredDir(path string) (vendoredModule, bool) {
	v, found := c.vendored[path]
	return v, found
}

func (c *collector) add(owner *moduleAdapter, moduleImport Import, disabled bool) (*moduleAdapter, error) {
	var (
		mod       *goModule
		moduleDir string
		version   string
		vendored  bool
	)

	modulePath := moduleImport.Path
	var realOwner Module = owner

	if !c.ignoreVendor {
		if err := c.collectModulesTXT(owner); err != nil {
			return nil, err
		}

		// Try _vendor first.
		var vm vendoredModule
		vm, vendored = c.getVendoredDir(modulePath)
		if vendored {
			moduleDir = vm.Dir
			realOwner = vm.Owner
			version = vm.Version

			if owner.projectMod {
				// We want to keep the go.mod intact with the versions and all.
				c.skipTidy = true
			}

		}
	}

	if moduleDir == "" {
		mod = c.gomods.GetByPath(modulePath)
		if mod != nil {
			moduleDir = mod.Dir
		}

		if moduleDir == "" {

			if c.GoModulesFilename != "" && c.isProbablyModule(modulePath) {
				// Try to "go get" it and reload the module configuration.
				if err := c.Get(modulePath); err != nil {
					return nil, err
				}
				if err := c.loadModules(); err != nil {
					return nil, err
				}

				mod = c.gomods.GetByPath(modulePath)
				if mod != nil {
					moduleDir = mod.Dir
				}
			}

			// Fall back to /themes/<mymodule>
			if moduleDir == "" {
				moduleDir = filepath.Join(c.themesDir, modulePath)

				if found, _ := afero.Exists(c.fs, moduleDir); !found {
					c.err = c.wrapModuleNotFound(errors.Errorf(`module %q not found; either add it as a Hugo Module or store it in %q.`, modulePath, c.themesDir))
					return nil, nil
				}
			}
		}
	}

	if found, _ := afero.Exists(c.fs, moduleDir); !found {
		c.err = c.wrapModuleNotFound(errors.Errorf("%q not found", moduleDir))
		return nil, nil
	}

	if !strings.HasSuffix(moduleDir, fileSeparator) {
		moduleDir += fileSeparator
	}

	ma := &moduleAdapter{
		dir:      moduleDir,
		vendor:   vendored,
		disabled: disabled,
		gomod:    mod,
		version:  version,
		// This may be the owner of the _vendor dir
		owner: realOwner,
	}

	if mod == nil {
		ma.path = modulePath
	}

	if err := ma.validateAndApplyDefaults(c.fs); err != nil {
		return nil, err
	}

	if !moduleImport.IgnoreConfig {
		if err := c.applyThemeConfig(ma); err != nil {
			return nil, err
		}
	}

	if err := c.applyMounts(moduleImport, ma); err != nil {
		return nil, err
	}

	c.modules = append(c.modules, ma)
	return ma, nil

}

func (c *collector) addAndRecurse(owner *moduleAdapter, disabled bool) error {
	moduleConfig := owner.Config()
	if owner.projectMod {
		if err := c.applyMounts(Import{}, owner); err != nil {
			return err
		}
	}

	for _, moduleImport := range moduleConfig.Imports {
		disabled := disabled || moduleImport.Disable

		if !c.isSeen(moduleImport.Path) {
			tc, err := c.add(owner, moduleImport, disabled)
			if err != nil {
				return err
			}
			if tc == nil {
				continue
			}
			if err := c.addAndRecurse(tc, disabled); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *collector) applyMounts(moduleImport Import, mod *moduleAdapter) error {
	mounts := moduleImport.Mounts

	if !mod.projectMod && len(mounts) == 0 {
		modConfig := mod.Config()
		mounts = modConfig.Mounts
		if len(mounts) == 0 {
			// Create default mount points for every component folder that
			// exists in the module.
			for _, componentFolder := range files.ComponentFolders {
				sourceDir := filepath.Join(mod.Dir(), componentFolder)
				_, err := c.fs.Stat(sourceDir)
				if err == nil {
					mounts = append(mounts, Mount{
						Source: componentFolder,
						Target: componentFolder,
					})
				}
			}
		}
	}

	var err error
	mounts, err = c.normalizeMounts(mod, mounts)
	if err != nil {
		return err
	}

	mod.mounts = mounts
	return nil
}

func (c *collector) applyThemeConfig(tc *moduleAdapter) error {

	var (
		configFilename string
		cfg            config.Provider
		themeCfg       map[string]interface{}
		hasConfig      bool
		err            error
	)

	// Viper supports more, but this is the sub-set supported by Hugo.
	for _, configFormats := range config.ValidConfigFileExtensions {
		configFilename = filepath.Join(tc.Dir(), "config."+configFormats)
		hasConfig, _ = afero.Exists(c.fs, configFilename)
		if hasConfig {
			break
		}
	}

	// The old theme information file.
	themeTOML := filepath.Join(tc.Dir(), "theme.toml")

	hasThemeTOML, _ := afero.Exists(c.fs, themeTOML)
	if hasThemeTOML {
		data, err := afero.ReadFile(c.fs, themeTOML)
		if err != nil {
			return err
		}
		themeCfg, err = metadecoders.Default.UnmarshalToMap(data, metadecoders.TOML)
		if err != nil {
			return errors.Wrapf(err, "failed to read module config for %q in %q", tc.Path(), themeTOML)
		}
		maps.ToLower(themeCfg)
	}

	if hasConfig {
		if configFilename != "" {
			var err error
			cfg, err = config.FromFile(c.fs, configFilename)
			if err != nil {
				return errors.Wrapf(err, "failed to read module config for %q in %q", tc.Path(), configFilename)
			}
		}

		tc.configFilename = configFilename
		tc.cfg = cfg
	}

	config, err := DecodeConfig(cfg)
	if err != nil {
		return err
	}

	const oldVersionKey = "min_version"

	if hasThemeTOML {

		// Merge old with new
		if minVersion, found := themeCfg[oldVersionKey]; found {
			if config.HugoVersion.Min == "" {
				config.HugoVersion.Min = hugo.VersionString(cast.ToString(minVersion))
			}
		}

		if config.Params == nil {
			config.Params = make(map[string]interface{})
		}

		for k, v := range themeCfg {
			if k == oldVersionKey {
				continue
			}
			config.Params[k] = v
		}

	}

	tc.config = config

	return nil

}

func (c *collector) collect() {
	if err := c.initModules(); err != nil {
		c.err = err
		return
	}

	projectMod := createProjectModule(c.gomods.GetMain(), c.workingDir, c.moduleConfig)

	if err := c.addAndRecurse(projectMod, false); err != nil {
		c.err = err
		return
	}

	// Append the project module at the tail.
	c.modules = append(c.modules, projectMod)

}

func (c *collector) collectModulesTXT(owner Module) error {
	vendorDir := filepath.Join(owner.Dir(), vendord)
	filename := filepath.Join(vendorDir, vendorModulesFilename)

	f, err := c.fs.Open(filename)

	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	defer f.Close()

	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		// # github.com/alecthomas/chroma v0.6.3
		line := scanner.Text()
		line = strings.Trim(line, "# ")
		line = strings.TrimSpace(line)
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return errors.Errorf("invalid modules list: %q", filename)
		}
		path := parts[0]
		if _, found := c.vendored[path]; !found {
			c.vendored[path] = vendoredModule{
				Owner:   owner,
				Dir:     filepath.Join(vendorDir, path),
				Version: parts[1],
			}
		}

	}
	return nil
}

func (c *collector) loadModules() error {
	modules, err := c.listGoMods()
	if err != nil {
		return err
	}
	c.gomods = modules
	return nil
}

func (c *collector) normalizeMounts(owner Module, mounts []Mount) ([]Mount, error) {
	var out []Mount
	dir := owner.Dir()

	for _, mnt := range mounts {
		errMsg := fmt.Sprintf("invalid module config for %q", owner.Path())

		if mnt.Source == "" || mnt.Target == "" {
			return nil, errors.New(errMsg + ": both source and target must be set")
		}

		mnt.Source = filepath.Clean(mnt.Source)
		mnt.Target = filepath.Clean(mnt.Target)

		// Verify that Source exists
		sourceDir := filepath.Join(dir, mnt.Source)
		_, err := c.fs.Stat(sourceDir)
		if err != nil {
			continue
		}

		// Verify that target points to one of the predefined component dirs
		targetBase := mnt.Target
		idxPathSep := strings.Index(mnt.Target, string(os.PathSeparator))
		if idxPathSep != -1 {
			targetBase = mnt.Target[0:idxPathSep]
		}
		if !files.IsComponentFolder(targetBase) {
			return nil, errors.Errorf("%s: mount target must be one of: %v", errMsg, files.ComponentFolders)
		}

		out = append(out, mnt)
	}

	return out, nil
}

func (c *collector) wrapModuleNotFound(err error) error {
	err = errors.Wrap(ErrNotExist, err.Error())
	if c.GoModulesFilename == "" {
		return err
	}

	baseMsg := "we found a go.mod file in your project, but"

	switch c.goBinaryStatus {
	case goBinaryStatusNotFound:
		return errors.Wrap(err, baseMsg+" you need to install Go to use it. See https://golang.org/dl/.")
	case goBinaryStatusTooOld:
		return errors.Wrap(err, baseMsg+" you need to a newer version of Go to use it. See https://golang.org/dl/.")
	}

	return err

}

type vendoredModule struct {
	Owner   Module
	Dir     string
	Version string
}

func createProjectModule(gomod *goModule, workingDir string, conf Config) *moduleAdapter {
	// Create a pseudo module for the main project.
	var path string
	if gomod == nil {
		path = "project"
	}

	return &moduleAdapter{
		path:       path,
		dir:        workingDir,
		gomod:      gomod,
		projectMod: true,
		config:     conf,
	}

}

// In the first iteration of Hugo Modules, we do not support multiple
// major versions running at the same time, so we pick the first (upper most).
// We will investigate namespaces in future versions.
// TODO(bep) add a warning when the above happens.
func pathKey(p string) string {
	prefix, _, _ := module.SplitPathVersion(p)

	return strings.ToLower(prefix)
}
