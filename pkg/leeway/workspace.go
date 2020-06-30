package leeway

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar"
	"github.com/imdario/mergo"
	log "github.com/sirupsen/logrus"
	"golang.org/x/xerrors"
	"gopkg.in/yaml.v3"
)

// Workspace is the root container of all compoments. All components are named relative
// to the origin of this workspace.
type Workspace struct {
	DefaultTarget    string            `yaml:"defaultTarget,omitempty"`
	ArgumentDefaults map[string]string `yaml:"defaultArgs,omitempty"`
	Variants         []*PackageVariant `yaml:"variants,omitempty"`

	Origin          string                `yaml:"-"`
	Components      map[string]*Component `yaml:"-"`
	Packages        map[string]*Package   `yaml:"-"`
	Scripts         map[string]*Script    `yaml:"-"`
	SelectedVariant *PackageVariant       `yaml:"-"`

	ignores []string
}

// ShouldIngoreComponent returns true if a file should be ignored for a component listing
func (ws *Workspace) ShouldIngoreComponent(path string) bool {
	return ws.ShouldIngoreSource(path)
}

// ShouldIngoreSource returns true if a file should be ignored for a source listing
func (ws *Workspace) ShouldIngoreSource(path string) bool {
	for _, ptn := range ws.ignores {
		if strings.Contains(path, ptn) {
			return true
		}
	}
	return false
}

// FindNestedWorkspaces loads nested workspaces
func FindNestedWorkspaces(path string, args Arguments, variant string) (res Workspace, err error) {
	wss, err := doublestar.Glob(filepath.Join(path, "**/WORKSPACE.yaml"))
	if err != nil {
		return
	}

	// deepest workspaces first
	sort.Slice(wss, func(i, j int) bool {
		return strings.Count(wss[i], string(os.PathSeparator)) > strings.Count(wss[j], string(os.PathSeparator))
	})

	loadedWorkspaces := make(map[string]*Workspace)
	for _, wspath := range wss {
		wspath = strings.TrimSuffix(strings.TrimSuffix(wspath, "WORKSPACE.yaml"), "/")
		log := log.WithField("wspath", wspath)
		log.Debug("loading (possibly nested) workspace")

		sws, err := loadWorkspace(wspath, args, variant, &loadWorkspaceOpts{
			PrelinkModifier: func(packages map[string]*Package) {
				for otherloc, otherws := range loadedWorkspaces {
					relativeOrigin := filepathTrimPrefix(otherloc, wspath)

					for k, p := range otherws.Packages {
						var otherKey string
						if strings.HasPrefix(k, "//") {
							otherKey = fmt.Sprintf("%s%s", relativeOrigin, strings.TrimPrefix(k, "//"))
						} else {
							otherKey = fmt.Sprintf("%s/%s", relativeOrigin, k)
						}
						packages[otherKey] = p

						log.WithField("relativeOrigin", relativeOrigin).WithField("package", otherKey).Debug("prelinking previously loaded workspace")
					}
				}
			},
		})
		if err != nil {
			return res, err
		}
		loadedWorkspaces[wspath] = &sws
		res = sws
	}

	// now that we've loaded and linked the main workspace, we need to fix the location names and indices
	nc := make(map[string]*Component)
	for _, pkg := range res.Packages {
		name := filepathTrimPrefix(pkg.C.Origin, res.Origin)
		if name == "" {
			name = "//"
		}
		pkg.C.Name = name
		nc[name] = pkg.C
		log.WithField("origin", pkg.C.Origin).WithField("name", name).Debug("renamed component")
	}
	res.Components = nc

	return
}

func filepathTrimPrefix(path, prefix string) string {
	return strings.TrimPrefix(strings.TrimPrefix(path, prefix), string(os.PathSeparator))
}

type loadWorkspaceOpts struct {
	PrelinkModifier func(map[string]*Package)
}

func loadWorkspace(path string, args Arguments, variant string, opts *loadWorkspaceOpts) (Workspace, error) {
	root := filepath.Join(path, "WORKSPACE.yaml")
	fc, err := ioutil.ReadFile(root)
	if err != nil {
		return Workspace{}, err
	}
	var workspace Workspace
	err = yaml.Unmarshal(fc, &workspace)
	if err != nil {
		return Workspace{}, err
	}
	workspace.Origin, err = filepath.Abs(filepath.Dir(root))
	if err != nil {
		return Workspace{}, err
	}

	if variant != "" {
		for _, vnt := range workspace.Variants {
			if vnt.Name == variant {
				workspace.SelectedVariant = vnt
				break
			}
		}
	}

	var ignores []string
	ignoresFile := filepath.Join(workspace.Origin, ".leewayignore")
	if _, err := os.Stat(ignoresFile); !os.IsNotExist(err) {
		fc, err := ioutil.ReadFile(ignoresFile)
		if err != nil {
			return Workspace{}, err
		}
		ignores = strings.Split(string(fc), "\n")
	}
	otherWS, err := doublestar.Glob(filepath.Join(workspace.Origin, "**/WORKSPACE.yaml"))
	if err != nil {
		return Workspace{}, err
	}
	for _, ows := range otherWS {
		dir := filepath.Dir(ows)
		if dir == workspace.Origin {
			continue
		}

		ignores = append(ignores, dir)
	}
	workspace.ignores = ignores
	log.WithField("ingores", workspace.ignores).Debug("computed workspace ignores")

	log.WithField("defaultArgs", workspace.ArgumentDefaults).Debug("applying workspace defaults")
	for key, val := range workspace.ArgumentDefaults {
		if args == nil {
			args = make(map[string]string)
		}

		_, alreadySet := args[key]
		if alreadySet {
			continue
		}

		args[key] = val
	}

	comps, err := discoverComponents(&workspace, args, workspace.SelectedVariant, opts)
	if err != nil {
		return workspace, err
	}
	workspace.Components = make(map[string]*Component)
	workspace.Packages = make(map[string]*Package)
	workspace.Scripts = make(map[string]*Script)
	for _, comp := range comps {
		workspace.Components[comp.Name] = comp

		for _, pkg := range comp.Packages {
			workspace.Packages[pkg.FullName()] = pkg
		}
		for _, script := range comp.Scripts {
			workspace.Scripts[script.FullName()] = script
		}
	}

	// now that we have all components/packages, we can link things
	if opts != nil && opts.PrelinkModifier != nil {
		opts.PrelinkModifier(workspace.Packages)
	}
	for _, pkg := range workspace.Packages {
		err := pkg.link(workspace.Packages)
		if err != nil {
			return workspace, xerrors.Errorf("linking error in package %s: %w", pkg.FullName(), err)
		}
	}
	for _, script := range workspace.Scripts {
		err := script.link(workspace.Packages)
		if err != nil {
			return workspace, xerrors.Errorf("linking error in script %s: %w", script.FullName(), err)
		}
	}

	// at this point all packages are fully loaded and we can compute the version, as well as resolve builtin variables
	for _, pkg := range workspace.Packages {
		err := pkg.resolveBuiltinVariables()
		if err != nil {
			return workspace, xerrors.Errorf("cannot resolve builtin variables %s: %w", pkg.FullName(), err)
		}
	}

	return workspace, nil
}

// FindWorkspace looks for a WORKSPACE.yaml file within the path. If multiple such files are found,
// an error is returned.
func FindWorkspace(path string, args Arguments, variant string) (Workspace, error) {
	return loadWorkspace(path, args, variant, &loadWorkspaceOpts{})
}

// discoverComponents discovers components in a workspace
func discoverComponents(workspace *Workspace, args Arguments, variant *PackageVariant, opts *loadWorkspaceOpts) ([]*Component, error) {
	path := workspace.Origin
	pths, err := doublestar.Glob(filepath.Join(path, "**/BUILD.yaml"))
	if err != nil {
		return nil, err
	}

	var comps []*Component
	for _, pth := range pths {
		if workspace.ShouldIngoreComponent(pth) {
			continue
		}

		comp, err := loadComponent(workspace, pth, args, variant)
		if err != nil {
			return nil, err
		}

		comps = append(comps, &comp)
	}

	return comps, nil
}

// loadComponent loads a component from a BUILD.yaml file
func loadComponent(workspace *Workspace, path string, args Arguments, variant *PackageVariant) (Component, error) {
	fc, err := ioutil.ReadFile(path)
	if err != nil {
		return Component{}, err
	}

	// we attempt to load the constants of a component first so that we can add it to the args
	var compconst struct {
		Constants Arguments `yaml:"const"`
	}
	err = yaml.Unmarshal(fc, &compconst)
	if err != nil {
		return Component{}, err
	}
	compargs := make(Arguments)
	for k, v := range args {
		compargs[k] = v
	}
	for k, v := range compconst.Constants {
		// constants overwrite args
		compargs[k] = v
	}

	// replace build args
	var rfc []byte = fc
	if len(args) > 0 {
		rfc = replaceBuildArguments(fc, compargs)
	}

	var (
		comp    Component
		rawcomp struct {
			Packages []yaml.Node
		}
	)
	err = yaml.Unmarshal(rfc, &comp)
	if err != nil {
		return comp, err
	}
	err = yaml.Unmarshal(fc, &rawcomp)
	if err != nil {
		return comp, err
	}

	name := strings.TrimPrefix(strings.TrimPrefix(filepath.Dir(path), workspace.Origin), "/")
	if name == "" {
		name = "//"
	}

	comp.W = workspace
	comp.Name = name
	comp.Origin = filepath.Dir(path)
	for i, pkg := range comp.Packages {
		pkg.C = &comp

		pkg.Definition, err = yaml.Marshal(&rawcomp.Packages[i])
		if err != nil {
			return comp, xerrors.Errorf("%s: %w", comp.Name, err)
		}

		pkg.Sources, err = resolveSources(pkg.C.W, pkg.C.Origin, pkg.Sources, false)
		if err != nil {
			return comp, xerrors.Errorf("%s: %w", comp.Name, err)
		}

		// add additional sources to package sources
		completeSources := make(map[string]struct{})
		for _, src := range pkg.Sources {
			completeSources[src] = struct{}{}
		}
		for _, src := range pkg.Config.AdditionalSources() {
			fn, err := filepath.Abs(filepath.Join(comp.Origin, src))
			if err != nil {
				return comp, xerrors.Errorf("%s: %w", comp.Name, err)
			}
			if _, err := os.Stat(fn); os.IsNotExist(err) {
				return comp, xerrors.Errorf("%s: %w", comp.Name, err)
			}
			if _, found := completeSources[fn]; found {
				continue
			}

			completeSources[fn] = struct{}{}
		}
		if vnt := pkg.C.W.SelectedVariant; vnt != nil {
			incl, excl, err := vnt.ResolveSources(pkg.C.W, pkg.C.Origin)
			if err != nil {
				return comp, xerrors.Errorf("%s: %w", comp.Name, err)
			}
			for _, i := range incl {
				completeSources[i] = struct{}{}
			}
			for _, i := range excl {
				delete(completeSources, i)
			}
			log.WithField("pkg", pkg.Name).WithField("variant", variant).WithField("excl", excl).WithField("incl", incl).WithField("package", pkg.FullName()).Debug("applying variant")
		}
		pkg.Sources = make([]string, len(completeSources))
		i := 0
		for src := range completeSources {
			pkg.Sources[i] = src
			i++
		}

		// re-set the version relevant arguments to <name>: <value>
		for i, argdep := range pkg.ArgumentDependencies {
			val, ok := args[argdep]
			if !ok {
				val = "<not-set>"
			}
			pkg.ArgumentDependencies[i] = fmt.Sprintf("%s: %s", argdep, val)
		}

		// make all dependencies fully qualified
		for idx, dep := range pkg.Dependencies {
			if !strings.HasPrefix(dep, ":") {
				continue
			}

			pkg.Dependencies[idx] = comp.Name + dep
		}

		// apply variant config
		if vnt := pkg.C.W.SelectedVariant; vnt != nil {
			err = mergeConfig(pkg, vnt.Config(pkg.Type))
			if err != nil {
				return comp, xerrors.Errorf("%s: %w", comp.Name, err)
			}

			err = mergeEnv(pkg, vnt.Environment)
			if err != nil {
				return comp, xerrors.Errorf("%s: %w", comp.Name, err)
			}
		}
	}

	for _, scr := range comp.Scripts {
		scr.C = &comp

		// fill in defaults
		if scr.Type == "" {
			scr.Type = BashScript
		}
		if scr.WorkdirLayout == "" {
			scr.WorkdirLayout = WorkdirOrigin
		}

		// make all dependencies fully qualified
		for idx, dep := range scr.Dependencies {
			if !strings.HasPrefix(dep, ":") {
				continue
			}

			scr.Dependencies[idx] = comp.Name + dep
		}
	}

	return comp, nil
}

func mergeConfig(pkg *Package, src PackageConfig) error {
	if src == nil {
		return nil
	}

	switch pkg.Config.(type) {
	case TypescriptPkgConfig:
		dst := pkg.Config.(TypescriptPkgConfig)
		in, ok := src.(TypescriptPkgConfig)
		if !ok {
			return xerrors.Errorf("cannot merge %s onto %s", reflect.TypeOf(src).String(), reflect.TypeOf(dst).String())
		}
		err := mergo.Merge(&dst, in)
		if err != nil {
			return err
		}
		pkg.Config = dst
	case GoPkgConfig:
		dst := pkg.Config.(GoPkgConfig)
		in, ok := src.(GoPkgConfig)
		if !ok {
			return xerrors.Errorf("cannot merge %s onto %s", reflect.TypeOf(src).String(), reflect.TypeOf(dst).String())
		}
		err := mergo.Merge(&dst, in)
		if err != nil {
			return err
		}
		pkg.Config = dst
	case DockerPkgConfig:
		dst := pkg.Config.(DockerPkgConfig)
		in, ok := src.(DockerPkgConfig)
		if !ok {
			return xerrors.Errorf("cannot merge %s onto %s", reflect.TypeOf(src).String(), reflect.TypeOf(dst).String())
		}
		err := mergo.Merge(&dst, in)
		if err != nil {
			return err
		}
		pkg.Config = dst
	case GenericPkgConfig:
		dst := pkg.Config.(GenericPkgConfig)
		in, ok := src.(GenericPkgConfig)
		if !ok {
			return xerrors.Errorf("cannot merge %s onto %s", reflect.TypeOf(src).String(), reflect.TypeOf(dst).String())
		}
		err := mergo.Merge(&dst, in)
		if err != nil {
			return err
		}
		pkg.Config = dst
	default:
		return xerrors.Errorf("unknown config type %s", reflect.ValueOf(pkg.Config).Elem().Type().String())
	}
	return nil
}

func mergeEnv(pkg *Package, src []string) error {
	env := make(map[string]string, len(pkg.Environment))
	for _, set := range [][]string{pkg.Environment, src} {
		for _, kv := range set {
			segs := strings.Split(kv, "=")
			if len(segs) < 2 {
				return xerrors.Errorf("environment variable must have format ENV=VAR: %s", kv)
			}

			env[segs[0]] = strings.Join(segs[1:], "=")
		}

	}

	pkg.Environment = make([]string, 0, len(env))
	for k, v := range env {
		pkg.Environment = append(pkg.Environment, fmt.Sprintf("%s=%s", k, v))
	}
	return nil
}