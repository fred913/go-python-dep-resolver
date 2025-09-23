package main

// PyPIPackageInfo represents the structure of PyPI package data
type PyPIPackageInfo struct {
	Info struct {
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		RequiresDist []string `json:"requires_dist"`
	} `json:"info"`
}

// Dependency represents a dependency with optional extras
type Dependency struct {
	Name   string   `json:"name"`
	Extras []string `json:"extras"`
}

// CachePackage represents the minimal info we want to persist
type CachePackage struct {
	Name             string                  `json:"name"`
	Version          string                  `json:"version"`
	Dependencies     []Dependency            `json:"dependencies"`
	DependencyGroups map[string][]Dependency `json:"dependency_groups"`
}
