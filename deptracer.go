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
}

// NewDependencyTracer creates a new tracer instance
func NewDependencyTracer(maxConcurrent int64, verbose bool) *DependencyTracer {
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
		maxConcurrent: maxConcurrent,
		verbose:       verbose,
	}
}

// ParseRequirements parses a requirements.txt file and extracts package names with extras
func (dt *DependencyTracer) ParseRequirements(filename string) ([]Dependency, map[string][]Dependency, error) {
	if dt.verbose {
		color.Blue("📖 Reading requirements file: %s", filename)
	}

	file, err := os.Open(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open requirements file: %w", err)
	}
	defer file.Close()

	var packages []Dependency
	dependencyGroups := make(map[string][]Dependency)
	packageRegex := regexp.MustCompile(`^([a-zA-Z0-9_-]+)(?:\[([a-zA-Z0-9_,-]+)\])?`)
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
			var extras []string
			if len(matches) > 2 && matches[2] != "" {
				// Parse extras like "security,dev,test"
				extrasStr := matches[2]
				for _, extra := range strings.Split(extrasStr, ",") {
					extras = append(extras, strings.TrimSpace(extra))
				}
			}

			dep := Dependency{
				Name:   packageName,
				Extras: extras,
			}
			packages = append(packages, dep)

			// Organize by groups - main group and each extra
			dependencyGroups["main"] = append(dependencyGroups["main"], dep)
			for _, extra := range extras {
				dependencyGroups[extra] = append(dependencyGroups[extra], dep)
			}

			if dt.verbose {
				if len(extras) > 0 {
					color.Green("  Line %d: Found package '%s' with extras %v from '%s'", lineNum, packageName, extras, line)
				} else {
					color.Green("  Line %d: Found package '%s' from '%s'", lineNum, packageName, line)
				}
			}
		} else if dt.verbose {
			color.Yellow("  Line %d: Could not parse package from '%s'", lineNum, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("error reading requirements file: %w", err)
	}

	return packages, dependencyGroups, nil
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

	const MAX_ATTEMPTS = 1

	// Retry logic with exponential backoff
	for attempt := 0; attempt < MAX_ATTEMPTS; attempt++ {
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
				return nil, fmt.Errorf("failed to fetch %s after %d attempts: %w", packageName, MAX_ATTEMPTS, err)
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
				depCount := len(dt.ExtractDependencies(&packageInfo, nil))
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

// ExtractDependencies extracts dependency names from package info and builds Dependency objects
func (dt *DependencyTracer) ExtractDependencies(packageInfo *PyPIPackageInfo, requestedExtras []string) []Dependency {
	var dependencies []Dependency
	dependencyRegex := regexp.MustCompile(`^([a-zA-Z0-9_-]+)`)

	for _, reqDist := range packageInfo.Info.RequiresDist {
		// Parse extra conditions from requires_dist like "requests ; extra == 'security'"
		var depExtras []string
		includesDep := true

		if strings.Contains(reqDist, ";") {
			parts := strings.Split(reqDist, ";")
			reqDist = strings.TrimSpace(parts[0]) // Get the dependency name part
			condition := strings.TrimSpace(parts[1])

			if strings.Contains(condition, "extra") {
				// Parse extra condition like "extra == 'security'" or "extra in ['dev', 'test']"
				if strings.Contains(condition, "extra ==") {
					// Single extra: extra == 'security'
					extraMatch := regexp.MustCompile(`extra\s*==\s*['"']([^'"]+)['"']`)
					if match := extraMatch.FindStringSubmatch(condition); len(match) > 1 {
						depExtras = []string{strings.TrimSpace(match[1])}
					}
				}

				// If we're not including optional deps and this has extras, skip unless requested
				if len(requestedExtras) > 0 {
					// Check if any of the dependency's extras are in the requested extras
					includesDep = false
					for _, reqExtra := range requestedExtras {
						for _, depExtra := range depExtras {
							if reqExtra == depExtra {
								includesDep = true
								break
							}
						}
						if includesDep {
							break
						}
					}
				}

				if dt.verbose && !includesDep {
					color.New(color.Faint).Printf("    Skipping conditional dependency: %s\n", reqDist)
				}
			}
		}

		if includesDep {
			matches := dependencyRegex.FindStringSubmatch(reqDist)
			if len(matches) > 1 {
				depName := strings.ToLower(matches[1])
				dep := Dependency{
					Name:   depName,
					Extras: depExtras,
				}
				dependencies = append(dependencies, dep)
				if dt.verbose {
					if len(depExtras) > 0 {
						color.New(color.Faint).Printf("    Found dependency: %s with extras %v (from %s)\n", depName, depExtras, reqDist)
					} else {
						color.New(color.Faint).Printf("    Found dependency: %s (from %s)\n", depName, reqDist)
					}
				}
			}
		}
	}

	return dependencies
}

// BuildDependencyGraph builds the dependency graph recursively with progress tracking
func (dt *DependencyTracer) BuildDependencyGraph(ctx context.Context, rootPackages []Dependency, maxDepth int) error {
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
			if visited.isVisited(pkg.dependency.Name) || pkg.depth >= maxDepth {
				producers.Done()
				continue
			}
			if visited.setVisited(pkg.dependency.Name) {
				producers.Done()
				continue
			}

			packageInfo, err := dt.FetchPackageInfo(ctx, pkg.dependency.Name)
			atomic.AddInt64(&processedCount, 1)
			bar.Add(1)

			if err == nil {
				deps := dt.ExtractDependencies(packageInfo, pkg.dependency.Extras)
				dt.dependencyGraph.Store(pkg.dependency.Name, deps)
				for _, dep := range deps {
					if !visited.isVisited(dep.Name) {
						producers.Add(1)
						jobs <- packageDepth{dependency: dep, depth: pkg.depth + 1}
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
			jobs <- packageDepth{dependency: pkg, depth: 0}
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
	dependency Dependency
	depth      int
}

// FindDependencyPaths finds all paths from root packages to target dependency
// FindDependencyPaths finds all unique paths from root packages to target dependency
func (dt *DependencyTracer) FindDependencyPaths(target string, rootPackages []Dependency) [][]string {
	color.Blue("🔍 Searching for dependency paths to '%s'...", target)

	var paths [][]string
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := make(map[string]struct{}) // deduplication set

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
		go func(rootPkg Dependency) {
			defer wg.Done()
			defer bar.Add(1)

			if dt.verbose {
				color.Blue("  🔎 Searching paths from %s to %s", rootPkg.Stringify(), target)
			}

			localPaths := dt.dfs(rootPkg, target, []string{}, make(map[string]bool), 10)

			if dt.verbose && len(localPaths) > 0 {
				color.Green("  ✅ Found %d path(s) from %s to %s", len(localPaths), rootPkg.Name, target)
			}

			mu.Lock()
			for _, p := range localPaths {
				key := strings.Join(p, " -> ")
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					paths = append(paths, p)
				}
			}
			mu.Unlock()
		}(root)
	}

	wg.Wait()
	bar.Finish()

	return paths
}

// dfs performs depth-first search to find paths
func (dt *DependencyTracer) dfs(current Dependency, target string, path []string, visited map[string]bool, maxDepth int) [][]string {
	if current.Name == target {
		result := make([]string, len(path)+1)
		copy(result, append(path, current.Stringify()))
		if dt.verbose {
			pathStr := strings.Join(result, " → ")
			color.Green("    🎯 Found path: %s", pathStr)
		}
		return [][]string{result}
	}

	if visited[current.Stringify()] || len(path) >= maxDepth {
		return nil
	}

	visited[current.Stringify()] = true
	defer func() { visited[current.Stringify()] = false }()

	var allPaths [][]string
	if deps, ok := dt.dependencyGraph.Load(current.Name); ok {
		dependencies := deps.([]Dependency)

		// Group by child name
		grouped := make(map[string]Dependency)
		for _, dep := range dependencies {
			grouped[dep.Name] = dep
		}

		// Recurse only once per child name
		for _, dep := range grouped {
			subPaths := dt.dfs(dep, target, append(path, current.Stringify()), visited, maxDepth)
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
		deps := dt.ExtractDependencies(pi, nil) // Export all dependencies, no filtering

		// Build dependency groups based on extras found in dependencies
		dependencyGroups := make(map[string][]Dependency)
		dependencyGroups["main"] = []Dependency{}

		for _, dep := range deps {
			// Add to main group
			dependencyGroups["main"] = append(dependencyGroups["main"], dep)

			// Add to specific extra groups if the dependency has extras
			for _, extra := range dep.Extras {
				if dependencyGroups[extra] == nil {
					dependencyGroups[extra] = []Dependency{}
				}
				dependencyGroups[extra] = append(dependencyGroups[extra], dep)
			}
		}

		data = append(data, CachePackage{
			Name:             name,
			Version:          pi.Info.Version,
			Dependencies:     deps,
			DependencyGroups: dependencyGroups,
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

		// Convert Dependencies back to requires_dist format for consistency
		var requiresDist []string
		for _, dep := range pkg.Dependencies {
			reqDist := dep.Name
			if len(dep.Extras) > 0 {
				reqDist += " ; extra == '" + strings.Join(dep.Extras, "' or extra == '") + "'"
			}
			requiresDist = append(requiresDist, reqDist)
		}
		pi.Info.RequiresDist = requiresDist

		dt.packageCache.Store(pkg.Name, pi)
		dt.dependencyGraph.Store(pkg.Name, pkg.Dependencies)

		// Store dependency groups if available
		if pkg.DependencyGroups != nil && len(pkg.DependencyGroups) > 0 {
			// Note: Dependency groups are package-level metadata from requirements parsing
			// For cached packages, we preserve the dependency structure but groups are
			// primarily relevant for root packages from requirements.txt parsing
			if dt.verbose {
				color.New(color.Faint).Printf("    Restored dependency groups for %s: %v\n", pkg.Name, getGroupNames(pkg.DependencyGroups))
			}
		}
	}

	if dt.verbose {
		color.Green("📥 Imported %d packages from cache file %s", len(pkgs), path)
	}
	return nil
}

// getGroupNames returns the keys of a dependency groups map for logging purposes
func getGroupNames(groups map[string][]Dependency) []string {
	var names []string
	for name := range groups {
		names = append(names, name)
	}
	return names
}

func (dt *DependencyTracer) ImportCache(cache map[string][]Dependency) {
	for name, deps := range cache {
		pi := &PyPIPackageInfo{}
		pi.Info.Name = name
		pi.Info.Version = "inmem"

		// Convert Dependencies back to requires_dist format
		var requiresDist []string
		for _, dep := range deps {
			reqDist := dep.Name
			if len(dep.Extras) > 0 {
				reqDist += " ; extra == '" + strings.Join(dep.Extras, "' or extra == '") + "'"
			}
			requiresDist = append(requiresDist, reqDist)
		}
		pi.Info.RequiresDist = requiresDist

		dt.packageCache.Store(name, pi)
		dt.dependencyGraph.Store(name, deps)
	}

	if dt.verbose {
		color.Green("📥 Imported %d packages into in-memory cache", len(cache))
	}
}
