package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatih/color"
	"github.com/schollz/progressbar/v3"
)

// DependencyTracer handles the dependency tracing logic
type DependencyTracer struct {
	client          *http.Client
	packageCache    sync.Map
	dependencyGraph sync.Map
	maxConcurrent   int64
	verbose         bool
	includeOptional bool
}

// NewDependencyTracer creates a new tracer instance
func NewDependencyTracer(maxConcurrent int64, verbose bool, includeOptional bool) *DependencyTracer {
	return &DependencyTracer{
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        int(maxConcurrent * 4),
				MaxIdleConnsPerHost: int(maxConcurrent * 2),
				IdleConnTimeout:     90 * time.Second,
				MaxConnsPerHost:     int(maxConcurrent * 2),
			},
		},
		maxConcurrent:   maxConcurrent,
		verbose:         verbose,
		includeOptional: includeOptional,
	}
}

// ParseRequirements parses a requirements.txt file and extracts package names
func (dt *DependencyTracer) ParseRequirements(filename string) ([]string, error) {
	if dt.verbose {
		color.Blue("📖 Reading requirements file: %s", filename)
	}

	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open requirements file: %w", err)
	}
	defer file.Close()

	var packages []string
	packageRegex := regexp.MustCompile(`^([a-zA-Z0-9_-]+)`)
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			if dt.verbose && strings.HasPrefix(line, "#") {
				color.New(color.Faint).Printf("  Line %d: Skipping comment\n", lineNum)
			}
			continue
		}

		matches := packageRegex.FindStringSubmatch(line)
		if len(matches) > 1 {
			packageName := strings.ToLower(matches[1])
			packages = append(packages, packageName)
			if dt.verbose {
				color.Green("  Line %d: Found package '%s' from '%s'", lineNum, packageName, line)
			}
		} else if dt.verbose {
			color.Yellow("  Line %d: Could not parse package from '%s'", lineNum, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading requirements file: %w", err)
	}

	return packages, nil
}

func (dt *DependencyTracer) FetchPackageInfoFast(packageName string) *PyPIPackageInfo {
	// Check cache first
	if cached, ok := dt.packageCache.Load(packageName); ok {
		return cached.(*PyPIPackageInfo)
	}
	return nil
}

// FetchPackageInfo fetches package information from PyPI with retry logic
func (dt *DependencyTracer) FetchPackageInfo(ctx context.Context, packageName string) (*PyPIPackageInfo, error) {
	fastResult := dt.FetchPackageInfoFast(packageName)
	if fastResult != nil {
		if dt.verbose {
			color.New(color.Faint).Printf("📦 Using fast (cached) dep data for %s\n", packageName)
		}
		return fastResult, nil
	}

	// Otherwise fetch from PyPI as before
	// Acquire semaphore to limit concurrency

	url := fmt.Sprintf("https://pypi.tuna.tsinghua.edu.cn/pypi/%s/json", packageName)

	if dt.verbose {
		color.Blue("🌐 Fetching %s from PyPI...", packageName)
	}

	// Retry logic with exponential backoff
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && dt.verbose {
			color.Yellow("  🔄 Retry attempt %d for %s", attempt+1, packageName)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}

		resp, err := dt.client.Do(req)
		if err != nil {
			if dt.verbose {
				color.Red("  ❌ Request failed for %s: %v", packageName, err)
			}
			if attempt == 2 {
				return nil, fmt.Errorf("failed to fetch %s after 3 attempts: %w", packageName, err)
			}
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			var packageInfo PyPIPackageInfo
			if err := json.NewDecoder(resp.Body).Decode(&packageInfo); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("failed to decode JSON for %s: %w", packageName, err)
			}
			resp.Body.Close()

			// Cache the result
			dt.packageCache.Store(packageName, &packageInfo)

			if dt.verbose {
				depCount := len(dt.ExtractDependencies(&packageInfo))
				color.Green("  ✅ Successfully fetched %s (v%s) with %d dependencies",
					packageInfo.Info.Name, packageInfo.Info.Version, depCount)
			}

			return &packageInfo, nil
		} else if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			if dt.verbose {
				color.Yellow("  ⏳ Rate limited for %s, waiting...", packageName)
			}
			if attempt < 2 {
				time.Sleep(time.Duration(2<<attempt) * time.Second)
				continue
			}
		}

		resp.Body.Close()
		if dt.verbose {
			color.Red("  ❌ HTTP %d for %s", resp.StatusCode, packageName)
		}

		if attempt == 2 {
			return nil, fmt.Errorf("HTTP %d for package %s", resp.StatusCode, packageName)
		}
		time.Sleep(time.Duration(1<<attempt) * time.Second)
	}

	return nil, fmt.Errorf("failed to fetch package %s", packageName)
}

// ExtractDependencies extracts dependency names from package info
func (dt *DependencyTracer) ExtractDependencies(packageInfo *PyPIPackageInfo) []string {
	var dependencies []string
	dependencyRegex := regexp.MustCompile(`^([a-zA-Z0-9_-]+)`)

	for _, reqDist := range packageInfo.Info.RequiresDist {
		// Skip conditional dependencies for simplicity
		if strings.Contains(reqDist, ";") && strings.Contains(reqDist, "extra") && !dt.includeOptional {
			if dt.verbose {
				color.New(color.Faint).Printf("    Skipping conditional dependency: %s\n", reqDist)
			}
			continue
		}

		matches := dependencyRegex.FindStringSubmatch(reqDist)
		if len(matches) > 1 {
			depName := strings.ToLower(matches[1])
			dependencies = append(dependencies, depName)
			if dt.verbose {
				color.New(color.Faint).Printf("    Found dependency: %s (from %s)\n", depName, reqDist)
			}
		}
	}

	return dependencies
}

// BuildDependencyGraph builds the dependency graph recursively with progress tracking
func (dt *DependencyTracer) BuildDependencyGraph(ctx context.Context, rootPackages []string, maxDepth int) error {
	visited := newVisitedMap()
	jobs := make(chan packageDepth, 10000)
	var wg sync.WaitGroup        // for workers
	var producers sync.WaitGroup // to know when to close jobs
	var processedCount int64

	bar := progressbar.NewOptions(-1,
		progressbar.OptionSetDescription("Processing packages"),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "█",
			SaucerHead:    "█",
			SaucerPadding: "░",
			BarStart:      "[",
			BarEnd:        "]",
		}),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetWidth(50),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionOnCompletion(func() {
			fmt.Println()
		}),
	)

	// Worker
	worker := func() {
		defer wg.Done()
		for pkg := range jobs {
			if visited.isVisited(pkg.name) || pkg.depth >= maxDepth {
				producers.Done()
				continue
			}
			if visited.setVisited(pkg.name) {
				producers.Done()
				continue
			}

			packageInfo, err := dt.FetchPackageInfo(ctx, pkg.name)
			atomic.AddInt64(&processedCount, 1)
			bar.Add(1)

			if err == nil {
				deps := dt.ExtractDependencies(packageInfo)
				dt.dependencyGraph.Store(pkg.name, deps)
				for _, dep := range deps {
					if !visited.isVisited(dep) {
						producers.Add(1)
						jobs <- packageDepth{name: dep, depth: pkg.depth + 1}
					}
				}
			}
			producers.Done()
		}
	}

	// Start workers
	for i := 0; i < int(dt.maxConcurrent); i++ {
		wg.Add(1)
		go worker()
	}

	// Seed roots
	producers.Add(len(rootPackages))
	go func() {
		for _, pkg := range rootPackages {
			jobs <- packageDepth{name: pkg, depth: 0}
		}
		producers.Wait() // wait until no more jobs will ever be added
		close(jobs)      // safe to close now
	}()

	wg.Wait()
	bar.Finish()

	total := 0
	dt.dependencyGraph.Range(func(_, _ interface{}) bool {
		total++
		return true
	})

	color.Green("✅ Dependency graph built: %d packages processed, %d unique packages in graph",
		processedCount, total)

	return nil
}

type packageDepth struct {
	name  string
	depth int
}

// FindDependencyPaths finds all paths from root packages to target dependency
func (dt *DependencyTracer) FindDependencyPaths(target string, rootPackages []string) [][]string {
	color.Blue("🔍 Searching for dependency paths to '%s'...", target)

	var paths [][]string
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Create progress bar for path finding
	bar := progressbar.NewOptions(len(rootPackages),
		progressbar.OptionSetDescription("Searching paths"),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "█",
			SaucerHead:    "█",
			SaucerPadding: "░",
			BarStart:      "[",
			BarEnd:        "]",
		}),
		progressbar.OptionShowCount(),
		progressbar.OptionSetWidth(50),
		progressbar.OptionOnCompletion(func() {
			fmt.Println()
		}),
	)

	for _, root := range rootPackages {
		wg.Add(1)
		go func(rootPkg string) {
			defer wg.Done()
			defer bar.Add(1)

			if dt.verbose {
				color.Blue("  🔎 Searching paths from %s to %s", rootPkg, target)
			}

			localPaths := dt.dfs(rootPkg, target, []string{}, make(map[string]bool), 10)

			if dt.verbose && len(localPaths) > 0 {
				color.Green("  ✅ Found %d path(s) from %s to %s", len(localPaths), rootPkg, target)
			}

			mu.Lock()
			paths = append(paths, localPaths...)
			mu.Unlock()
		}(root)
	}

	wg.Wait()
	bar.Finish()

	return paths
}

// dfs performs depth-first search to find paths
func (dt *DependencyTracer) dfs(current, target string, path []string, visited map[string]bool, maxDepth int) [][]string {
	if current == target {
		result := make([]string, len(path)+1)
		copy(result, append(path, current))
		if dt.verbose {
			pathStr := strings.Join(result, " → ")
			color.Green("    🎯 Found path: %s", pathStr)
		}
		return [][]string{result}
	}

	if visited[current] || len(path) >= maxDepth {
		return nil
	}

	visited[current] = true
	defer func() { visited[current] = false }()

	var allPaths [][]string
	if deps, ok := dt.dependencyGraph.Load(current); ok {
		dependencies := deps.([]string)
		for _, dep := range dependencies {
			subPaths := dt.dfs(dep, target, append(path, current), visited, maxDepth)
			allPaths = append(allPaths, subPaths...)
		}
	}

	return allPaths
}

// DisplayDependencyTree displays the dependency paths
func (dt *DependencyTracer) DisplayDependencyTree(paths [][]string, target string) {
	fmt.Println()
	if len(paths) == 0 {
		color.Red("❌ No dependency paths found to '%s'", target)
		color.Yellow("💡 This might mean:")
		color.Yellow("   • The package is not used by any of your requirements")
		color.Yellow("   • The package is beyond the maximum search depth")
		color.Yellow("   • There was an error fetching some package information")
		return
	}

	color.Green("🎉 Found %d path(s) to '%s':\n", len(paths), target)

	for i, path := range paths {
		color.Cyan("📍 Path %d (%d steps):", i+1, len(path)-1)
		if len(path) > 1 {
			arrowPath := strings.Join(path, " → ")
			fmt.Printf("   %s\n", arrowPath)
		} else {
			fmt.Printf("   %s (direct dependency)\n", path[0])
		}
		fmt.Println()
	}
}

func (dt *DependencyTracer) ExportCacheJson(path string) error {
	var data []CachePackage

	dt.packageCache.Range(func(key, value interface{}) bool {
		name := key.(string)
		pi := value.(*PyPIPackageInfo)

		// Use ExtractDependencies so we store clean names
		deps := dt.ExtractDependencies(pi)

		data = append(data, CachePackage{
			Name:         name,
			Version:      pi.Info.Version,
			Dependencies: deps,
		})
		return true
	})

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create cache file: %w", err)
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	if dt.verbose {
		color.Green("💾 Exported %d packages to cache file %s", len(data), path)
	}
	return nil
}

func (dt *DependencyTracer) ImportCacheJson(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read cache file: %w", err)
	}

	var pkgs []CachePackage
	if err := json.Unmarshal(data, &pkgs); err != nil {
		return fmt.Errorf("failed to parse cache file: %w", err)
	}

	for _, pkg := range pkgs {
		pi := &PyPIPackageInfo{}
		pi.Info.Name = pkg.Name
		pi.Info.Version = pkg.Version
		pi.Info.RequiresDist = pkg.Dependencies
		dt.packageCache.Store(pkg.Name, pi)
		dt.dependencyGraph.Store(pkg.Name, pkg.Dependencies)
	}

	if dt.verbose {
		color.Green("📥 Imported %d packages from cache file %s", len(pkgs), path)
	}
	return nil
}

func (dt *DependencyTracer) ImportCache(cache map[string][]string) {
	for name, deps := range cache {
		pi := &PyPIPackageInfo{}
		pi.Info.Name = name
		pi.Info.Version = "inmem"
		pi.Info.RequiresDist = deps
		dt.packageCache.Store(name, pi)
		dt.dependencyGraph.Store(name, deps)
	}

	if dt.verbose {
		color.Green("📥 Imported %d packages into in-memory cache", len(cache))
	}
}
