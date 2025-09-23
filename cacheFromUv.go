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
			Name string `toml:"name"`
		} `toml:"dependencies"`
	} `toml:"package"`
}

func ParseUvLock(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read uv.lock: %w", err)
	}

	var lock UvLock
	if err := toml.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("failed to parse uv.lock: %w", err)
	}

	cache := make(map[string][]string)
	for _, pkg := range lock.Packages {
		var deps []string
		for _, d := range pkg.Dependencies {
			deps = append(deps, strings.ToLower(d.Name))
		}
		cache[strings.ToLower(pkg.Name)] = deps
	}
	return cache, nil
}
