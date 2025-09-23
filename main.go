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
	"github.com/spf13/cobra"
)

// PyPIPackageInfo represents the structure of PyPI package data
type PyPIPackageInfo struct {
	Info struct {
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		RequiresDist []string `json:"requires_dist"`
	} `json:"info"`
}

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

// FetchPackageInfo fetches package information from PyPI with retry logic
func (dt *DependencyTracer) FetchPackageInfo(ctx context.Context, packageName string) (*PyPIPackageInfo, error) {
	// Check cache first
	if cached, ok := dt.packageCache.Load(packageName); ok {
		if dt.verbose {
			color.New(color.Faint).Printf("📦 Using cached data for %s\n", packageName)
		}
		return cached.(*PyPIPackageInfo), nil
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

// Thread-safe visited map wrapper
type visitedMap struct {
	mu sync.RWMutex
	m  map[string]bool
}

func newVisitedMap() *visitedMap {
	return &visitedMap{
		m: make(map[string]bool),
	}
}

func (vm *visitedMap) isVisited(key string) bool {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	return vm.m[key]
}

func (vm *visitedMap) setVisited(key string) bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.m[key] {
		return true // already visited
	}
	vm.m[key] = true
	return false // newly visited
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

// CLI Commands
var rootCmd = &cobra.Command{
	Use:   "pypi-tracer",
	Short: "Trace PyPI dependency chains from requirements.txt",
	Long:  "A fast tool to trace dependency chains from requirements.txt to find which packages lead to a target dependency through recursive resolution.",
}

var traceCmd = &cobra.Command{
	Use:   "trace",
	Short: "Trace dependency chains to find paths to a target package",
	Long:  "Trace dependency chains from requirements.txt to find all paths that lead to a target dependency.",
	RunE: func(cmd *cobra.Command, args []string) error {
		reqsFile, _ := cmd.Flags().GetString("file")
		pattern, _ := cmd.Flags().GetString("pattern")
		maxDepth, _ := cmd.Flags().GetInt("max-depth")
		maxConcurrent, _ := cmd.Flags().GetInt64("concurrency")
		verbose, _ := cmd.Flags().GetBool("verbose")
		cacheFile, _ := cmd.Flags().GetString("cache")
		includeOptional, _ := cmd.Flags().GetBool("include-optional")

		if pattern == "" {
			return fmt.Errorf("pattern is required")
		}

		if _, err := os.Stat(reqsFile); os.IsNotExist(err) {
			color.Red("❌ Error: Requirements file '%s' not found", reqsFile)
			return err
		}

		startTime := time.Now()
		color.Blue("🚀 Starting PyPI dependency tracer...")
		color.Blue("   📁 Requirements file: %s", reqsFile)
		color.Blue("   🎯 Target package: %s", pattern)
		color.Blue("   📊 Max depth: %d", maxDepth)
		color.Blue("   ⚡ Concurrency: %d", maxConcurrent)
		if verbose {
			color.Blue("   🔧 Verbose mode: enabled")
		}
		if verbose {
			color.Blue("   🔧 Including optional dependencies: enabled")
		}
		fmt.Println()

		tracer := NewDependencyTracer(maxConcurrent, verbose, includeOptional)

		if cacheFile != "" {
			color.Blue("📂 Loading local cache from %s", cacheFile)
			localCache, err := ParseUvLock(cacheFile)
			if err != nil {
				return err
			}
			// tracer.localCache = localCache
			// Apply them to packageCache

			// Check uv.lock local cache
			// if deps, ok := dt.localCache[packageName]; ok {
			// 	pi := &PyPIPackageInfo{}
			// 	pi.Info.Name = packageName
			// 	pi.Info.Version = "cached"
			// 	pi.Info.RequiresDist = nil // optional
			// 	dt.packageCache.Store(packageName, pi)
			// 	return pi, nil
			// }
			for packageName, deps := range localCache {
				pi := &PyPIPackageInfo{}
				pi.Info.Name = packageName
				pi.Info.Version = "cached"
				pi.Info.RequiresDist = deps
				tracer.packageCache.Store(packageName, pi)
			}

			color.Green("✅ Loaded %d cached packages from uv.lock", len(localCache))
		} else {
			color.Yellow("⚠️ No local cache file provided, fetching all packages might be a bit slow")
		}
		ctx := context.Background()

		// Parse requirements.txt
		color.Blue("📖 Step 1: Parsing requirements file...")
		rootPackages, err := tracer.ParseRequirements(reqsFile)
		if err != nil {
			return err
		}

		if len(rootPackages) == 0 {
			color.Red("❌ No packages found in requirements file")
			return fmt.Errorf("no packages found")
		}

		color.Green("✅ Found %d root package(s): %s\n", len(rootPackages), strings.Join(rootPackages, ", "))

		// Build dependency graph
		color.Blue("🔄 Step 2: Building dependency graph...")
		if err := tracer.BuildDependencyGraph(ctx, rootPackages, maxDepth); err != nil {
			return err
		}

		// Find paths to target
		color.Blue("🔍 Step 3: Finding dependency paths...")
		paths := tracer.FindDependencyPaths(strings.ToLower(pattern), rootPackages)

		// Display results
		tracer.DisplayDependencyTree(paths, pattern)

		// Show final summary
		totalPackages := 0
		tracer.dependencyGraph.Range(func(key, value interface{}) bool {
			totalPackages++
			return true
		})

		elapsed := time.Since(startTime)
		color.New(color.Bold).Printf("📈 Summary:\n")
		fmt.Printf("   • Total packages analyzed: %d\n", totalPackages)
		fmt.Printf("   • Dependency paths found: %d\n", len(paths))
		fmt.Printf("   • Total execution time: %.2fs\n", elapsed.Seconds())
		fmt.Printf("   • Average processing rate: %.1f packages/sec\n",
			float64(totalPackages)/elapsed.Seconds())

		return nil
	},
}

func init() {
	traceCmd.Flags().StringP("file", "f", "requirements.txt", "Path to requirements.txt file")
	traceCmd.Flags().StringP("pattern", "p", "", "Target dependency to trace (required)")
	traceCmd.Flags().IntP("max-depth", "d", 3, "Maximum depth for dependency resolution")
	traceCmd.Flags().Int64P("concurrency", "c", 128, "Maximum concurrent requests")
	traceCmd.Flags().BoolP("verbose", "v", false, "Enable verbose logging")
	traceCmd.Flags().String("cache", "", "Path to uv.lock TOML cache file")
	traceCmd.Flags().Bool("include-optional", false, "Include optional dependencies in the graph")

	rootCmd.AddCommand(traceCmd)
	// rootCmd.AddCommand(createSampleCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		color.Red("❌ Error: %v", err)
		os.Exit(1)
	}
}
