package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// UvLock mirrors uv.lock schema
type UvLock struct {
	Packages []struct {
		Name         string `toml:"name"`
		Dependencies []struct {
			Name   string   `toml:"name"`
			Extras []string `toml:"extras,omitempty"`
		} `toml:"dependencies"`
		// optional-dependencies
		OptionalDependencies map[string][]struct {
			Name   string   `toml:"name"`
			Extras []string `toml:"extras,omitempty"`
		}
	} `toml:"package"`
}

func ParseUvLock(path string) (map[string]DependencyGraphItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read uv.lock: %w", err)
	}

	var lock UvLock
	if err := toml.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("failed to parse uv.lock: %w", err)
	}

	cache := make(map[string]DependencyGraphItem)
	for _, pkg := range lock.Packages {
		deps := make([]Dependency, 0)
		depGroups := make(map[string][]Dependency)
		for _, d := range pkg.Dependencies {
			dep := Dependency{
				Name:   strings.ToLower(d.Name),
				Extras: d.Extras,
			}
			deps = append(deps, dep)
		}
		for groupName, groupDeps := range pkg.OptionalDependencies {
			group := make([]Dependency, 0)
			for _, d := range groupDeps {
				dep := Dependency{
					Name:   strings.ToLower(d.Name),
					Extras: d.Extras,
				}
				group = append(group, dep)
			}
			depGroups[groupName] = group
		}
		cache[strings.ToLower(pkg.Name)] = DependencyGraphItem{
			deps,
			depGroups,
		}
	}
	return cache, nil
}
