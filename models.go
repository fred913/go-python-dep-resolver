package main

// PyPIPackageInfo represents the structure of PyPI package data
type PyPIPackageInfo struct {
	Info struct {
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		RequiresDist []string `json:"requires_dist"`
	} `json:"info"`
}
