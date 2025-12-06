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
  desktopci upload --channel <ch> --artifacts <dir> --endpoint <url> --bucket <name> --version <ver> --pub-date <iso> [--flatpak-gpg-public-key <base64>]`)
}

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

func cmdBumpWeb() error {
	const tagPrefix = "web-build-"
	const tagGlob = "web-build-*"
	const defaultStart = 1000
	const maxRetries = 10

	if err := gitConfig("user.name", "github-actions[bot]"); err != nil {
		return err
	}
	if err := gitConfig("user.email", "41898282+github-actions[bot]@users.noreply.github.com"); err != nil {
		return err
	}

	for attempt := 0; attempt < maxRetries; attempt++ {
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

		if err := gitTag(tagName); err != nil {
			continue
		}

		if err := gitPushTag(tagName); err != nil {
			_ = exec.Command("git", "tag", "-d", tagName).Run()
			fmt.Printf("Tag %s already exists on remote; retrying with next number (attempt %d/%d)\n", tagName, attempt+1, maxRetries)
			continue
		}

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

		fmt.Printf("Successfully created and pushed tag %s\n", tagName)
		return nil
	}

	return fmt.Errorf("failed to create web build tag after %d attempts", maxRetries)
}

func cmdUpload(args []string) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	channel := fs.String("channel", "", "channel (stable/canary)")
	artifactsDir := fs.String("artifacts", "artifacts", "artifacts root")
	endpoint := fs.String("endpoint", "", "S3 endpoint URL")
	bucket := fs.String("bucket", "", "S3 bucket name")
	version := fs.String("version", "", "version string")
	pubDate := fs.String("pub-date", "", "ISO8601 pub date")
	flatpakGPGPublicKey := fs.String("flatpak-gpg-public-key", "", "Base64-encoded GPG public key for .flatpakref files")
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

	type artifact struct {
		id          string
		label       string
		dir         string
		patterns    []string
		key         string
		contentType string
	}

	uploadArtifacts := func(items []artifact) (map[string]string, error) {
		found := make(map[string]string)

		for _, a := range items {
			fullPatterns := make([]string, len(a.patterns))
			for i, p := range a.patterns {
				fullPatterns[i] = filepath.Join(a.dir, p)
			}

			path, err := findFirst(fullPatterns...)
			if err != nil {
				fmt.Printf("Warning: %s not found: %v\n", a.label, err)
				continue
			}

			if err := uploader(path, a.key, a.contentType); err != nil {
				return found, fmt.Errorf("upload %s: %w", a.label, err)
			}

			if a.id != "" {
				found[a.id] = path
			}
		}
		return found, nil
	}

	winX64Dir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-windows-x64", *channel))
	winArm64Dir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-windows-arm64", *channel))
	macDir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-macos-universal", *channel))
	linuxX64Dir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-linux-x64", *channel))
	linuxArm64Dir := filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-linux-arm64", *channel))

	found, err := uploadArtifacts([]artifact{
		{
			id:          "win-x64-exe",
			label:       "Windows x64 exe",
			dir:         winX64Dir,
			patterns:    []string{"*-x64-setup.exe", "*-setup.exe", "*.exe", "**/*.exe"},
			key:         fmt.Sprintf("%s/windows/x64/fluxer.exe", *channel),
			contentType: "application/vnd.microsoft.portable-executable",
		},
		{
			label:       "Windows arm64 exe",
			dir:         winArm64Dir,
			patterns:    []string{"*-arm64-setup.exe", "*-setup.exe", "*.exe", "**/*.exe"},
			key:         fmt.Sprintf("%s/windows/arm64/fluxer.exe", *channel),
			contentType: "application/vnd.microsoft.portable-executable",
		},
		{
			id:          "mac-dmg",
			label:       "macOS dmg",
			dir:         macDir,
			patterns:    []string{"*.dmg", "**/*.dmg"},
			key:         fmt.Sprintf("%s/macos/universal/fluxer.dmg", *channel),
			contentType: "application/x-apple-diskimage",
		},
		{
			id:          "mac-zip",
			label:       "macOS zip",
			dir:         macDir,
			patterns:    []string{"*.zip", "**/*.zip"},
			key:         fmt.Sprintf("%s/macos/universal/fluxer.zip", *channel),
			contentType: "application/zip",
		},
		{
			id:          "linux-x64-appimage",
			label:       "Linux x64 AppImage",
			dir:         linuxX64Dir,
			patterns:    []string{"*.AppImage", "**/*.AppImage"},
			key:         fmt.Sprintf("%s/linux/x64/fluxer.AppImage", *channel),
			contentType: "application/x-executable",
		},
		{
			label:       "Linux x64 deb",
			dir:         linuxX64Dir,
			patterns:    []string{"*.deb", "**/*.deb"},
			key:         fmt.Sprintf("%s/linux/x64/fluxer.deb", *channel),
			contentType: "application/vnd.debian.binary-package",
		},
		{
			label:       "Linux x64 tar.gz",
			dir:         linuxX64Dir,
			patterns:    []string{"*.tar.gz", "**/*.tar.gz"},
			key:         fmt.Sprintf("%s/linux/x64/fluxer.tar.gz", *channel),
			contentType: "application/gzip",
		},
		{
			label:       "Linux arm64 AppImage",
			dir:         linuxArm64Dir,
			patterns:    []string{"*.AppImage", "**/*.AppImage"},
			key:         fmt.Sprintf("%s/linux/arm64/fluxer.AppImage", *channel),
			contentType: "application/x-executable",
		},
		{
			label:       "Linux arm64 deb",
			dir:         linuxArm64Dir,
			patterns:    []string{"*.deb", "**/*.deb"},
			key:         fmt.Sprintf("%s/linux/arm64/fluxer.deb", *channel),
			contentType: "application/vnd.debian.binary-package",
		},
		{
			label:       "Linux arm64 tar.gz",
			dir:         linuxArm64Dir,
			patterns:    []string{"*.tar.gz", "**/*.tar.gz"},
			key:         fmt.Sprintf("%s/linux/arm64/fluxer.tar.gz", *channel),
			contentType: "application/gzip",
		},
	})
	if err != nil {
		return err
	}

	if macZip := found["mac-zip"]; macZip != "" {
		macYml, err := generateLatestYml(macZip, *version, *pubDate, "universal")
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
	} else if macDmg := found["mac-dmg"]; macDmg != "" {
		macYml, err := generateLatestYml(macDmg, *version, *pubDate, "universal")
		if err != nil {
			return fmt.Errorf("generate latest-mac.yml from dmg: %w", err)
		}
		tmp := filepath.Join(os.TempDir(), "latest-mac.yml")
		if err := os.WriteFile(tmp, macYml, 0o644); err != nil {
			return err
		}
		if err := uploader(tmp, fmt.Sprintf("desktop/%s/latest-mac.yml", *channel), "text/yaml"); err != nil {
			return fmt.Errorf("upload latest-mac.yml from dmg: %w", err)
		}
	}

	if winX64Exe := found["win-x64-exe"]; winX64Exe != "" {
		winYml, err := generateLatestYml(winX64Exe, *version, *pubDate, "x64")
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

	if linuxX64AppImage := found["linux-x64-appimage"]; linuxX64AppImage != "" {
		linuxYml, err := generateLatestYml(linuxX64AppImage, *version, *pubDate, "x64")
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

	flatpakRepoDirs := []string{
		filepath.Join(linuxX64Dir, "fluxer_app", "flatpak", "repo", *channel),
		filepath.Join(linuxArm64Dir, "fluxer_app", "flatpak", "repo", *channel),
	}

	var flatpakRepoDir string
	for _, dir := range flatpakRepoDirs {
		if _, err := os.Stat(dir); err == nil {
			flatpakRepoDir = dir
			break
		}
	}

	if flatpakRepoDir != "" {
		fmt.Printf("Uploading Flatpak repository from %s\n", flatpakRepoDir)
		if err := uploadFlatpakRepo(uploader, flatpakRepoDir, *channel); err != nil {
			return fmt.Errorf("upload flatpak repo: %w", err)
		}

		if err := uploadFlatpakRefs(uploader, *channel, *flatpakGPGPublicKey); err != nil {
			return fmt.Errorf("upload flatpak refs: %w", err)
		}

		fmt.Println("Flatpak repository uploaded successfully!")
	} else {
		fmt.Println("No Flatpak repository found, skipping Flatpak upload")
	}

	return nil
}

func generateLatestYml(artifactPath, version, pubDate, arch string) ([]byte, error) {
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

	info, err := os.Stat(artifactPath)
	if err != nil {
		return nil, err
	}

	filename := filepath.Base(artifactPath)
	ext := getArtifactExtension(filename)
	relativePath := fmt.Sprintf("fluxer-%s-%s.%s", version, arch, ext)

	latest := map[string]any{
		"version":     version,
		"releaseDate": pubDate,
		"files": []map[string]any{
			{
				"url":    relativePath,
				"sha512": sha512Hash,
				"size":   info.Size(),
			},
		},
		"path":         relativePath,
		"sha512":       sha512Hash,
		"releaseNotes": "",
	}

	return yaml.Marshal(latest)
}

func getArtifactExtension(filename string) string {
	lower := strings.ToLower(filename)
	extensions := []string{".tar.gz", ".exe", ".dmg", ".zip", ".appimage", ".deb"}

	for _, ext := range extensions {
		if strings.HasSuffix(lower, ext) {
			return strings.TrimPrefix(ext, ".")
		}
	}

	return "bin"
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

// uploadFlatpakRepo recursively uploads an OSTree repository to S3
func uploadFlatpakRepo(uploader func(string, string, string) error, repoDir, channel string) error {
	return filepath.Walk(repoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(repoDir, path)
		if err != nil {
			return err
		}

		s3Key := fmt.Sprintf("flatpak/%s/repo/%s", channel, filepath.ToSlash(relPath))

		contentType := getOSTreeContentType(relPath)

		fmt.Printf("  Uploading %s\n", s3Key)
		return uploader(path, s3Key, contentType)
	})
}

// getOSTreeContentType returns the appropriate content type for OSTree repo files
func getOSTreeContentType(path string) string {
	lower := strings.ToLower(path)

	switch {
	case strings.HasSuffix(lower, ".xml"):
		return "application/xml"
	case strings.HasSuffix(lower, ".sig"):
		return "application/pgp-signature"
	case strings.HasPrefix(filepath.Base(lower), "config"):
		return "text/plain"
	case strings.HasPrefix(filepath.Base(lower), "summary"):
		return "application/octet-stream"
	default:
		return "application/octet-stream"
	}
}

// uploadFlatpakRefs generates and uploads .flatpakref files for easy installation
func uploadFlatpakRefs(uploader func(string, string, string) error, channel, gpgPublicKey string) error {
	appID := "app.fluxer.Fluxer"
	if channel == "canary" {
		appID = "app.fluxer.FluxerCanary"
	}

	arches := []string{"x86_64", "aarch64"}

	for _, arch := range arches {
		refContent := generateFlatpakRef(appID, channel, gpgPublicKey)

		tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("fluxer-%s-%s.flatpakref", channel, arch))
		if err := os.WriteFile(tmpFile, []byte(refContent), 0o644); err != nil {
			return fmt.Errorf("write flatpakref temp file: %w", err)
		}
		defer os.Remove(tmpFile)

		s3Key := fmt.Sprintf("flatpak/%s/%s/fluxer-%s-%s.flatpakref", channel, arch, channel, arch)
		fmt.Printf("  Uploading %s\n", s3Key)
		if err := uploader(tmpFile, s3Key, "application/vnd.flatpak.ref"); err != nil {
			return fmt.Errorf("upload flatpakref %s: %w", arch, err)
		}
	}

	return nil
}

// generateFlatpakRef creates the content of a .flatpakref file
func generateFlatpakRef(appID, channel, gpgPublicKey string) string {
	var builder strings.Builder

	builder.WriteString("[Flatpak Ref]\n")
	builder.WriteString(fmt.Sprintf("Name=%s\n", appID))
	builder.WriteString(fmt.Sprintf("Branch=%s\n", channel))
	builder.WriteString(fmt.Sprintf("Url=https://api.fluxer.app/dl/flatpak/%s/repo\n", channel))
	builder.WriteString("IsRuntime=false\n")

	if gpgPublicKey != "" {
		builder.WriteString(fmt.Sprintf("GPGKey=%s\n", gpgPublicKey))
	}

	builder.WriteString("RuntimeRepo=https://dl.flathub.org/repo/flathub.flatpakrepo\n")

	return builder.String()
}
