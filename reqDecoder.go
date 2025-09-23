package main

import (
	"regexp"
	"strings"

	"github.com/fatih/color"
)

// RequirementEntry represents a single dependency requirement
type RequirementEntry struct {
	Name            string   // Package name (normalized to lowercase)
	TargetExtra     []string // Extras requested for this package (e.g., [security, dev])
	IncludedInExtra *string  // Which extra group this dependency belongs to (nil for main/unconditional)
}

// RequirementsDecoder handles parsing of both requirements.txt lines and PyPI requires_dist entries
type RequirementsDecoder struct {
	// Regex patterns for parsing
	packageRegex regexp.Regexp // For requirements.txt: package[extras]
	nameRegex    regexp.Regexp // For requires_dist: extract just the name part
	extraRegex   regexp.Regexp // For requires_dist: extract extra conditions
	verbose      bool
}

// NewRequirementsDecoder creates a new decoder instance
func NewRequirementsDecoder(verbose bool) *RequirementsDecoder {
	return &RequirementsDecoder{
		packageRegex: *regexp.MustCompile(`^([a-zA-Z0-9_-]+)(?:\[([a-zA-Z0-9_,-]+)\])?`),
		nameRegex:    *regexp.MustCompile(`^([a-zA-Z0-9_-]+)`),
		extraRegex:   *regexp.MustCompile(`extra\s*==\s*['"']([^'"]+)['"']`),
		verbose:      verbose,
	}
}

// DecodeRequirementsLine parses a requirements.txt line
func (rd *RequirementsDecoder) DecodeRequirementsLine(line string) []RequirementEntry {
	line = strings.TrimSpace(line)

	// Skip empty lines and comments
	if line == "" || strings.HasPrefix(line, "#") {
		if rd.verbose && strings.HasPrefix(line, "#") {
			color.New(color.Faint).Printf("  Skipping comment\n")
		}
		return nil
	}

	matches := rd.packageRegex.FindStringSubmatch(line)
	if len(matches) <= 1 {
		if rd.verbose {
			color.Yellow("  Could not parse package from '%s'", line)
		}
		return nil
	}

	packageName := strings.ToLower(matches[1])
	var targetExtras []string

	// Parse extras like "security,dev,test"
	if len(matches) > 2 && matches[2] != "" {
		for _, extra := range strings.Split(matches[2], ",") {
			targetExtras = append(targetExtras, strings.TrimSpace(extra))
		}
	}

	entry := RequirementEntry{
		Name:            packageName,
		TargetExtra:     targetExtras,
		IncludedInExtra: nil, // Requirements.txt entries are always main dependencies
	}

	if rd.verbose {
		if len(targetExtras) > 0 {
			color.Green("  Found package '%s' with extras %v from '%s'", packageName, targetExtras, line)
		} else {
			color.Green("  Found package '%s' from '%s'", packageName, line)
		}
	}

	return []RequirementEntry{entry}
}

// DecodeRequiresDistLine parses a PyPI requires_dist entry
func (rd *RequirementsDecoder) DecodeRequiresDistLine(requiresDist string) []RequirementEntry {
	// Split requirement and condition
	parts := strings.SplitN(requiresDist, ";", 2)
	depName := strings.TrimSpace(parts[0])

	// Extract dependency name
	matches := rd.nameRegex.FindStringSubmatch(depName)
	if len(matches) == 0 {
		return nil
	}

	name := strings.ToLower(matches[1])
	var includedInExtra *string

	// Handle conditional dependencies
	if len(parts) > 1 {
		condition := strings.TrimSpace(parts[1])
		if strings.Contains(condition, "extra") {
			// Extract extra condition
			if match := rd.extraRegex.FindStringSubmatch(condition); len(match) > 1 {
				extraName := strings.TrimSpace(match[1])
				includedInExtra = &extraName
			}
		}
	}

	entry := RequirementEntry{
		Name:            name,
		TargetExtra:     nil, // PyPI entries don't specify target extras
		IncludedInExtra: includedInExtra,
	}

	if rd.verbose {
		if includedInExtra != nil {
			color.New(color.Faint).Printf("    Found dependency: %s included in extra '%s' (from %s)\n", name, *includedInExtra, requiresDist)
		} else {
			color.New(color.Faint).Printf("    Found dependency: %s (from %s)\n", name, requiresDist)
		}
	}

	return []RequirementEntry{entry}
}

// ConvertToLegacyDependency converts RequirementEntry to the existing Dependency struct for backward compatibility
func (entry RequirementEntry) ConvertToLegacyDependency() Dependency {
	var extras []string

	// For requirements.txt entries, extras come from TargetExtra
	if entry.TargetExtra != nil {
		extras = entry.TargetExtra
	}

	// For requires_dist entries, extras come from IncludedInExtra
	if entry.IncludedInExtra != nil {
		extras = []string{*entry.IncludedInExtra}
	}

	return Dependency{
		Name:   entry.Name,
		Extras: extras,
	}
}
