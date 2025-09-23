package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

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
