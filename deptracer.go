package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
	dependencyGraph sync.Map // map[string]DependencyGraphItem
	maxConcurrent   int64
	verbose         bool
	reqDecoder      *RequirementsDecoder
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
		reqDecoder:    NewRequirementsDecoder(false),
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

	var dependencies []Dependency
	dependencyGroups := make(map[string][]Dependency)

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		matches := dt.reqDecoder.DecodeRequirementsLine(line)
		for _, match := range matches {
			if match.IncludedInExtra == nil {
				dep := Dependency{
					Name:   match.Name,
					Extras: match.TargetExtra,
				}
				dependencies = append(dependencies, dep)
			} else {
				dependencyGroups[*match.IncludedInExtra] = append(dependencyGroups[*match.IncludedInExtra], Dependency{
					Name:   match.Name,
					Extras: match.TargetExtra,
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("error reading requirements file: %w", err)
	}

	return dependencies, dependencyGroups, nil
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
				deps, dGrps := dt.ExtractDependencies(&packageInfo)
				depCount := len(deps)
				oDepCount := CountDependencyGroupPackages(dGrps)
				color.Green("  ✅ Successfully fetched %s (v%s) with %d+%d dependencies",
					packageInfo.Info.Name, packageInfo.Info.Version, depCount, oDepCount)
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
func (dt *DependencyTracer) ExtractDependencies(packageInfo *PyPIPackageInfo) ([]Dependency, map[string][]Dependency) {
	dependencies := make([]Dependency, 0)
	dependencyGroups := make(map[string][]Dependency)
	for _, req := range packageInfo.Info.RequiresDist {
		matches := dt.reqDecoder.DecodeRequiresDistLine(req)
		for _, match := range matches {
			if match.IncludedInExtra == nil {
				dep := Dependency{
					Name:   match.Name,
					Extras: match.TargetExtra,
				}
				dependencies = append(dependencies, dep)
			} else {
				dependencyGroups[*match.IncludedInExtra] = append(dependencyGroups[*match.IncludedInExtra], Dependency{
					Name:   match.Name,
					Extras: match.TargetExtra,
				})
			}
		}
	}
	return dependencies, dependencyGroups
}

// Helper function to check if any dependency extra matches requested extras
func hasAnyOverlap(arrA, arrB []string) bool {
	for _, depExtra := range arrA {
		for _, reqExtra := range arrB {
			if depExtra == reqExtra {
				return true
			}
		}
	}
	return false
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
				deps, groupedDeps := dt.ExtractDependencies(packageInfo)
				dt.dependencyGraph.Store(packageInfo.Info.Name, DependencyGraphItem{
					deps,
					groupedDeps,
				})
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
	if dependencies, err := dt.composeDeps(current); err == nil {
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

func (dt *DependencyTracer) composeDeps(current Dependency) ([]Dependency, error) {
	dgItemRaw, ok := dt.dependencyGraph.Load(current.Name)
	if !ok {
		return nil, fmt.Errorf("package %s not found in dependency graph", current.Name)
	}
	dgItem := dgItemRaw.(DependencyGraphItem)
	dependencies := make([]Dependency, 0)
	dependencies = append(dependencies, dgItem.Dependencies...)
	for _, groupExtraLabel := range current.Extras {
		group, ok := dgItem.DependencyGroups[groupExtraLabel]
		if !ok {
			return nil, fmt.Errorf("package %s doesn't have group %s, but we're asserting its existance", current.Name, groupExtraLabel)
		}

		for _, dep := range group {
			if hasAnyOverlap(dep.Extras, current.Extras) {
				dependencies = append(dependencies, dep)
			}
		}
	}
	return dependencies, nil
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
		deps, dependencyGroups := dt.ExtractDependencies(pi)

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
		dt.dependencyGraph.Store(pkg.Name, pkg.DependencyGraphItem())
	}

	if dt.verbose {
		color.Green("📥 Imported %d packages from cache file %s", len(pkgs), path)
	}
	return nil
}

func (dt *DependencyTracer) ImportCache(cache map[string]DependencyGraphItem) {
	for name, deps := range cache {
		pi := &PyPIPackageInfo{}
		pi.Info.Name = name
		pi.Info.Version = "inmem"

		// Convert Dependencies back to requires_dist format
		var requiresDist []string
		for _, dep := range deps.Dependencies {
			reqDist := dep.Name
			if len(dep.Extras) > 0 {
				reqDist += "[" + strings.Join(dep.Extras, ",") + "]"
			}
			requiresDist = append(requiresDist, reqDist)
		}
		for groupName, group := range deps.DependencyGroups {
			for _, dep := range group {
				reqDist := dep.Name
				if len(dep.Extras) > 0 {
					reqDist += "[" + strings.Join(dep.Extras, ",") + "]"
				}
				reqDist += " ; extra == '" + groupName + "'"
				requiresDist = append(requiresDist, reqDist)
			}
		}
		pi.Info.RequiresDist = requiresDist

		dt.packageCache.Store(name, pi)
		deps, depGrps := dt.ExtractDependencies(pi)
		dt.dependencyGraph.Store(pi.Info.Name, DependencyGraphItem{
			deps,
			depGrps,
		})
	}

	if dt.verbose {
		color.Green("📥 Imported %d packages into in-memory cache", len(cache))
	}
}
