/*
 * Copyright (C) 2025 Fluxer Contributors
 *
 * This file is part of Fluxer.
 *
 * Fluxer is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Fluxer is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Fluxer. If not, see <https://www.gnu.org/licenses/>.
 */

package main

import (
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "bump":
		if err := cmdBump(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "bump-web":
		if err := cmdBumpWeb(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "upload":
		if err := cmdUpload(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`Usage:
  desktopci bump
  desktopci bump-web
  desktopci upload --channel <ch> --artifacts <dir> --endpoint <url> --bucket <name> --version <ver> --pub-date <iso>`)
}

// bump: increments v0.0.x tags, writes version/pub_date to GITHUB_OUTPUT, and tags/pushes.
func cmdBump() error {
	tagGlob := envOr("TAG_GLOB", "v0.0.*")
	tagPrefix := envOr("TAG_PREFIX", "v")

	if err := gitFetchTags(); err != nil {
		return err
	}

	latest, _ := latestTag(tagGlob)
	num := 0
	if latest != "" {
		trimmed := strings.TrimPrefix(latest, tagPrefix)
		parts := strings.Split(trimmed, ".")
		if len(parts) >= 3 {
			parsed, err := strconv.Atoi(parts[2])
			if err != nil {
				return fmt.Errorf("parse latest tag %q: %w", latest, err)
			}
			num = parsed
		}
	}
	num++
	version := fmt.Sprintf("0.0.%d", num)
	tagName := fmt.Sprintf("%s%s", tagPrefix, version)
	pubDate := time.Now().UTC().Format(time.RFC3339)

	if out := os.Getenv("GITHUB_OUTPUT"); out != "" {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Fprintf(f, "version=%s\n", version)
		fmt.Fprintf(f, "pub_date=%s\n", pubDate)
	} else {
		fmt.Printf("version=%s\n", version)
		fmt.Printf("pub_date=%s\n", pubDate)
	}

	if err := gitConfig("user.name", "github-actions[bot]"); err != nil {
		return err
	}
	if err := gitConfig("user.email", "41898282+github-actions[bot]@users.noreply.github.com"); err != nil {
		return err
	}

	if err := gitTagExists(tagName); err == nil {
		fmt.Printf("Tag %s already exists; skipping push\n", tagName)
		return nil
	}
	if err := gitTag(tagName); err != nil {
		return err
	}
	return gitPushTag(tagName)
}

// bump-web: increments web-build-X tags, writes build_number to GITHUB_OUTPUT, and tags/pushes.
// Uses a global incrementing integer starting at 1000 (first build is 1001).
// Shared between canary and stable web deployments.
func cmdBumpWeb() error {
	const tagPrefix = "web-build-"
	const tagGlob = "web-build-*"
	const defaultStart = 1000

	if err := gitFetchTags(); err != nil {
		return err
	}

	latest, _ := latestTag(tagGlob)
	num := defaultStart
	if latest != "" {
		trimmed := strings.TrimPrefix(latest, tagPrefix)
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return fmt.Errorf("parse latest web tag %q: %w", latest, err)
		}
		num = parsed
	}
	num++
	tagName := fmt.Sprintf("%s%d", tagPrefix, num)

	if out := os.Getenv("GITHUB_OUTPUT"); out != "" {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Fprintf(f, "build_number=%d\n", num)
	} else {
		fmt.Printf("build_number=%d\n", num)
	}

	if err := gitConfig("user.name", "github-actions[bot]"); err != nil {
		return err
	}
	if err := gitConfig("user.email", "41898282+github-actions[bot]@users.noreply.github.com"); err != nil {
		return err
	}

	if err := gitTagExists(tagName); err == nil {
		fmt.Printf("Tag %s already exists; skipping push\n", tagName)
		return nil
	}
	if err := gitTag(tagName); err != nil {
		return err
	}
	return gitPushTag(tagName)
}

// cmdUpload uploads Electron build artifacts to S3.
// Electron-builder generates latest.yml files that electron-updater uses.
func cmdUpload(args []string) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	channel := fs.String("channel", "", "channel (stable/canary)")
	artifactsDir := fs.String("artifacts", "artifacts", "artifacts root")
	endpoint := fs.String("endpoint", "", "S3 endpoint URL")
	bucket := fs.String("bucket", "", "S3 bucket name")
	version := fs.String("version", "", "version string")
	pubDate := fs.String("pub-date", "", "ISO8601 pub date")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *channel == "" || *endpoint == "" || *bucket == "" || *version == "" || *pubDate == "" {
		return errors.New("channel, endpoint, bucket, version, pub-date are required")
	}

	uploader := func(src, key, contentType string) error {
		args := []string{"s3", "cp", src, fmt.Sprintf("s3://%s/%s", *bucket, key), "--endpoint-url", *endpoint}
		if contentType != "" {
			args = append(args, "--content-type", contentType)
		}
		cmd := exec.Command("aws", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	findFirst := func(patterns ...string) (string, error) {
		for _, pattern := range patterns {
			matches, err := filepath.Glob(pattern)
			if err != nil {
				continue
			}
			if len(matches) > 0 {
				return matches[0], nil
			}
		}
		return "", fmt.Errorf("no file found matching patterns: %v", patterns)
	}

	// Windows x64
	winX64Dir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-windows-x64", *channel))
	winX64Exe, err := findFirst(
		filepath.Join(winX64Dir, "*.exe"),
		filepath.Join(winX64Dir, "**", "*.exe"),
	)
	if err != nil {
		fmt.Printf("Warning: Windows x64 exe not found: %v\n", err)
	} else {
		if err := uploader(winX64Exe, fmt.Sprintf("%s/windows/x64/%s", *channel, filepath.Base(winX64Exe)), "application/vnd.microsoft.portable-executable"); err != nil {
			return fmt.Errorf("upload windows x64 exe: %w", err)
		}
	}

	// Windows arm64
	winArm64Dir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-windows-arm64", *channel))
	winArm64Exe, err := findFirst(
		filepath.Join(winArm64Dir, "*.exe"),
		filepath.Join(winArm64Dir, "**", "*.exe"),
	)
	if err != nil {
		fmt.Printf("Warning: Windows arm64 exe not found: %v\n", err)
	} else {
		if err := uploader(winArm64Exe, fmt.Sprintf("%s/windows/arm64/%s", *channel, filepath.Base(winArm64Exe)), "application/vnd.microsoft.portable-executable"); err != nil {
			return fmt.Errorf("upload windows arm64 exe: %w", err)
		}
	}

	// macOS universal
	macDir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-macos-universal", *channel))
	macDmg, err := findFirst(
		filepath.Join(macDir, "*.dmg"),
		filepath.Join(macDir, "**", "*.dmg"),
	)
	if err != nil {
		fmt.Printf("Warning: macOS dmg not found: %v\n", err)
	} else {
		if err := uploader(macDmg, fmt.Sprintf("%s/macos/universal/%s", *channel, filepath.Base(macDmg)), "application/x-apple-diskimage"); err != nil {
			return fmt.Errorf("upload macos dmg: %w", err)
		}
	}

	macZip, err := findFirst(
		filepath.Join(macDir, "*.zip"),
		filepath.Join(macDir, "**", "*.zip"),
	)
	if err != nil {
		fmt.Printf("Warning: macOS zip not found: %v\n", err)
	} else {
		if err := uploader(macZip, fmt.Sprintf("%s/macos/universal/%s", *channel, filepath.Base(macZip)), "application/zip"); err != nil {
			return fmt.Errorf("upload macos zip: %w", err)
		}
	}

	// Linux x64
	linuxX64Dir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-linux-x64", *channel))
	linuxX64AppImage, err := findFirst(
		filepath.Join(linuxX64Dir, "*.AppImage"),
		filepath.Join(linuxX64Dir, "**", "*.AppImage"),
	)
	if err != nil {
		fmt.Printf("Warning: Linux x64 AppImage not found: %v\n", err)
	} else {
		if err := uploader(linuxX64AppImage, fmt.Sprintf("%s/linux/x64/%s", *channel, filepath.Base(linuxX64AppImage)), "application/x-executable"); err != nil {
			return fmt.Errorf("upload linux x64 appimage: %w", err)
		}
	}

	// Linux arm64
	linuxArm64Dir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-linux-arm64", *channel))
	linuxArm64AppImage, err := findFirst(
		filepath.Join(linuxArm64Dir, "*.AppImage"),
		filepath.Join(linuxArm64Dir, "**", "*.AppImage"),
	)
	if err != nil {
		fmt.Printf("Warning: Linux arm64 AppImage not found: %v\n", err)
	} else {
		if err := uploader(linuxArm64AppImage, fmt.Sprintf("%s/linux/arm64/%s", *channel, filepath.Base(linuxArm64AppImage)), "application/x-executable"); err != nil {
			return fmt.Errorf("upload linux arm64 appimage: %w", err)
		}
	}

	// Generate and upload latest.yml files for each platform
	// electron-updater expects platform-specific latest.yml files

	// latest-mac.yml
	if macZip != "" {
		macYml, err := generateLatestYml(macZip, *version, *pubDate, *channel, "macos", "universal")
		if err != nil {
			return fmt.Errorf("generate latest-mac.yml: %w", err)
		}
		tmp := filepath.Join(os.TempDir(), "latest-mac.yml")
		if err := os.WriteFile(tmp, macYml, 0o644); err != nil {
			return err
		}
		if err := uploader(tmp, fmt.Sprintf("desktop/%s/latest-mac.yml", *channel), "text/yaml"); err != nil {
			return fmt.Errorf("upload latest-mac.yml: %w", err)
		}
	}

	// latest.yml (Windows - electron-updater default for Windows)
	if winX64Exe != "" {
		winYml, err := generateLatestYml(winX64Exe, *version, *pubDate, *channel, "windows", "x64")
		if err != nil {
			return fmt.Errorf("generate latest.yml (windows): %w", err)
		}
		tmp := filepath.Join(os.TempDir(), "latest.yml")
		if err := os.WriteFile(tmp, winYml, 0o644); err != nil {
			return err
		}
		if err := uploader(tmp, fmt.Sprintf("desktop/%s/latest.yml", *channel), "text/yaml"); err != nil {
			return fmt.Errorf("upload latest.yml: %w", err)
		}
	}

	// latest-linux.yml
	if linuxX64AppImage != "" {
		linuxYml, err := generateLatestYml(linuxX64AppImage, *version, *pubDate, *channel, "linux", "x64")
		if err != nil {
			return fmt.Errorf("generate latest-linux.yml: %w", err)
		}
		tmp := filepath.Join(os.TempDir(), "latest-linux.yml")
		if err := os.WriteFile(tmp, linuxYml, 0o644); err != nil {
			return err
		}
		if err := uploader(tmp, fmt.Sprintf("desktop/%s/latest-linux.yml", *channel), "text/yaml"); err != nil {
			return fmt.Errorf("upload latest-linux.yml: %w", err)
		}
	}

	fmt.Println("Electron artifacts uploaded successfully!")
	return nil
}

// generateLatestYml creates a latest.yml file for electron-updater
func generateLatestYml(artifactPath, version, pubDate, channel, platform, arch string) ([]byte, error) {
	// Compute SHA512 hash
	f, err := os.Open(artifactPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hash := sha512.New()
	if _, err := io.Copy(hash, f); err != nil {
		return nil, err
	}
	sha512Hash := base64.StdEncoding.EncodeToString(hash.Sum(nil))

	// Get file info
	info, err := os.Stat(artifactPath)
	if err != nil {
		return nil, err
	}

	filename := filepath.Base(artifactPath)
	downloadURL := fmt.Sprintf("https://api.fluxer.app/dl/%s/%s/%s?channel=%s", platform, arch, getArtifactType(filename), channel)

	latest := map[string]any{
		"version":     version,
		"releaseDate": pubDate,
		"files": []map[string]any{
			{
				"url":    downloadURL,
				"sha512": sha512Hash,
				"size":   info.Size(),
			},
		},
		"path":         downloadURL,
		"sha512":       sha512Hash,
		"releaseNotes": "",
	}

	return yaml.Marshal(latest)
}

// getArtifactType returns a URL-friendly artifact type from filename
func getArtifactType(filename string) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".exe"):
		return "setup"
	case strings.HasSuffix(lower, ".dmg"):
		return "dmg"
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	case strings.HasSuffix(lower, ".appimage"):
		return "appimage"
	default:
		return "file"
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func gitFetchTags() error {
	return exec.Command("git", "fetch", "--tags", "--force").Run()
}

func latestTag(glob string) (string, error) {
	cmd := exec.Command("git", "tag", "-l", glob, "--sort=-v:refname")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", nil
	}
	return lines[0], nil
}

func gitConfig(key, value string) error {
	return exec.Command("git", "config", key, value).Run()
}

func gitTagExists(tag string) error {
	return exec.Command("git", "rev-parse", tag).Run()
}

func gitTag(tag string) error {
	return exec.Command("git", "tag", tag).Run()
}

func gitPushTag(tag string) error {
	cmd := exec.Command("git", "push", "origin", tag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
