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
	} `toml:"package"`
}

func ParseUvLock(path string) (map[string][]Dependency, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read uv.lock: %w", err)
	}

	var lock UvLock
	if err := toml.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("failed to parse uv.lock: %w", err)
	}

	cache := make(map[string][]Dependency)
	for _, pkg := range lock.Packages {
		var deps []Dependency
		for _, d := range pkg.Dependencies {
			dep := Dependency{
				Name:   strings.ToLower(d.Name),
				Extras: d.Extras, // Use extras from uv.lock if available
			}
			deps = append(deps, dep)
		}
		cache[strings.ToLower(pkg.Name)] = deps
	}
	return cache, nil
}
