package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// localModule is an explicitly selected source tree, never a downloaded module.
type localModule struct {
	name      string
	directory string
	root      *os.Root
}

type dependencyReport struct {
	Module    string          `json:"module"`
	Directory string          `json:"directory"`
	Packages  []packageReport `json:"packages"`
}

func canonicalRelative(name string) bool {
	return name != "" && name != "." && name == path.Clean(name) && !path.IsAbs(name) && name != ".." &&
		!strings.HasPrefix(name, "../") &&
		!strings.Contains(name, "\\")
}

func validModuleName(name string) bool {
	return canonicalRelative(name) && !strings.ContainsAny(name, ":=\"") && !strings.ContainsFunc(name, unicode.IsSpace)
}

func readModule(root *os.Root) (string, error) {
	data, err := root.ReadFile("go.mod")
	if err != nil {
		return "", err
	}
	module := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		line, _, _ = strings.Cut(line, "//")
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "module" {
			continue
		}
		if len(fields) != 2 || module != "" {
			return "", errors.New("invalid module directive in go.mod")
		}
		module = fields[1]
		if strings.HasPrefix(module, `"`) {
			module, err = strconv.Unquote(module)
			if err != nil {
				return "", fmt.Errorf("invalid module directive: %w", err)
			}
		}
	}
	if !validModuleName(module) {
		return "", errors.New("invalid or missing module directive in go.mod")
	}
	return module, nil
}

func openMappedModule(repo *os.Root, mapping string) (localModule, error) {
	name, directory, ok := strings.Cut(mapping, "=")
	if !ok || !validModuleName(name) || !canonicalRelative(directory) {
		return localModule{}, fmt.Errorf(
			"invalid module mapping %q: expected import=canonical-repo-relative-directory",
			mapping,
		)
	}
	if err := rejectSymlinks(repo, filepath.FromSlash(directory)); err != nil {
		return localModule{}, fmt.Errorf("module mapping %q: %w", mapping, err)
	}
	root, err := repo.OpenRoot(filepath.FromSlash(directory))
	if err != nil {
		return localModule{}, err
	}
	declared, err := readModule(root)
	if err != nil || declared != name {
		if err == nil {
			err = fmt.Errorf("go.mod declares %q, expected %q", declared, name)
		}
		return localModule{}, errors.Join(err, root.Close())
	}
	return localModule{name: name, directory: directory, root: root}, nil
}

// openModules opens the measured root module followed by each validated mapping.
// On error it closes every root it opened.
func openModules(repo string, mappings []string) (modules []localModule, err error) {
	repoRoot, err := openRepositoryRoot(repo)
	if err != nil {
		return nil, err
	}
	modules = []localModule{{directory: ".", root: repoRoot}}
	defer func() {
		if err != nil {
			err = errors.Join(err, closeModules(modules))
			modules = nil
		}
	}()
	if modules[0].name, err = readModule(repoRoot); err != nil {
		return modules, err
	}
	for _, mapping := range mappings {
		mapped, openErr := openMappedModule(repoRoot, mapping)
		if openErr != nil {
			return modules, openErr
		}
		duplicate := duplicateModule(modules, mapped)
		modules = append(modules, mapped)
		if duplicate {
			return modules, fmt.Errorf("duplicate module mapping %q", mapping)
		}
	}
	return modules, nil
}

func closeModules(modules []localModule) error {
	var err error
	for _, module := range modules {
		err = errors.Join(err, module.root.Close())
	}
	return err
}

func moduleForFile(modules []localModule, filename string) int {
	selected := -1
	for i, module := range modules {
		if strings.HasPrefix(filename, module.name+"/") &&
			(selected < 0 || len(module.name) > len(modules[selected].name)) {
			selected = i
		}
	}
	return selected
}

func rejectSymlinks(root *os.Root, filename string) error {
	// Root confines every lookup/read even if a component changes after Lstat.
	for component := filename; component != "."; component = filepath.Dir(component) {
		info, err := root.Lstat(component)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlinked source paths are not allowed")
		}
	}
	return nil
}
