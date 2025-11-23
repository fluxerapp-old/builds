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
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
	case "set-tauri":
		if err := cmdSetTauri(os.Args[2:]); err != nil {
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
  desktopci set-tauri --config <path> --version <ver>
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
			fmt.Sscanf(parts[2], "%d", &num)
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

func cmdSetTauri(args []string) error {
	fs := flag.NewFlagSet("set-tauri", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to tauri config json")
	version := fs.String("version", "", "version to set")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *version == "" {
		return errors.New("config and version are required")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	obj["version"] = *version
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(*configPath, out, 0o644)
}

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

	type findSpec struct {
		dir     string
		pattern string
	}
	findOne := func(spec findSpec) (string, error) {
		matches, err := filepath.Glob(filepath.Join(spec.dir, spec.pattern))
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return "", fmt.Errorf("missing artifact matching %s", filepath.Join(spec.dir, spec.pattern))
		}
		return matches[0], nil
	}

	winX64Setup, err := findOne(findSpec{filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-windows-x64/nsis", *channel)), "*_x64-setup.exe"})
	if err != nil {
		return err
	}
	winX64Sig, err := readSig(winX64Setup + ".sig")
	if err != nil {
		return err
	}

	winArmSetup, err := findOne(findSpec{filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-windows-arm64/nsis", *channel)), "*_arm64-setup.exe"})
	if err != nil {
		return err
	}
	winArmSig, err := readSig(winArmSetup + ".sig")
	if err != nil {
		return err
	}

	macDmg, err := findOne(findSpec{filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-macos-universal/dmg", *channel)), "*_universal.dmg"})
	if err != nil {
		return err
	}

	macTarGz, err := findOne(findSpec{filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-macos-universal/macos", *channel)), "*.app.tar.gz"})
	if err != nil {
		return err
	}
	macSig, err := readSig(macTarGz + ".sig")
	if err != nil {
		return err
	}

	linX64, err := findOne(findSpec{filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-linux-x64/appimage", *channel)), "*.AppImage"})
	if err != nil {
		return err
	}
	linX64Sig, err := readSig(linX64 + ".sig")
	if err != nil {
		return err
	}

	linArm, err := findOne(findSpec{filepath.Join(*artifactsDir, fmt.Sprintf("fluxer-desktop-%s-linux-arm64/appimage", *channel)), "*.AppImage"})
	if err != nil {
		return err
	}
	linArmSig, err := readSig(linArm + ".sig")
	if err != nil {
		return err
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

	if err := uploader(winX64Setup, fmt.Sprintf("%s/windows/x64/fluxer.exe", *channel), "application/vnd.microsoft.portable-executable"); err != nil {
		return err
	}
	if err := uploader(winArmSetup, fmt.Sprintf("%s/windows/arm64/fluxer.exe", *channel), "application/vnd.microsoft.portable-executable"); err != nil {
		return err
	}
	if err := uploader(macDmg, fmt.Sprintf("%s/macos/universal/fluxer.dmg", *channel), "application/x-apple-diskimage"); err != nil {
		return err
	}
	if err := uploader(macTarGz, fmt.Sprintf("%s/macos/universal/fluxer.app.tar.gz", *channel), "application/gzip"); err != nil {
		return err
	}
	if err := uploader(linX64, fmt.Sprintf("%s/linux/x64/fluxer.AppImage", *channel), "application/x-executable"); err != nil {
		return err
	}
	if err := uploader(linArm, fmt.Sprintf("%s/linux/arm64/fluxer.AppImage", *channel), "application/x-executable"); err != nil {
		return err
	}

	latest := map[string]any{
		"version":  *version,
		"notes":    "",
		"pub_date": *pubDate,
		"platforms": map[string]any{
			"windows-x86_64": map[string]string{
				"url":       fmt.Sprintf("https://api.fluxer.app/dl/windows/x64/setup?channel=%s", *channel),
				"signature": winX64Sig,
			},
			"windows-aarch64": map[string]string{
				"url":       fmt.Sprintf("https://api.fluxer.app/dl/windows/arm64/setup?channel=%s", *channel),
				"signature": winArmSig,
			},
			"darwin-x86_64": map[string]string{
				"url":       fmt.Sprintf("https://api.fluxer.app/dl/macos/universal/app_tar_gz?channel=%s", *channel),
				"signature": macSig,
			},
			"darwin-aarch64": map[string]string{
				"url":       fmt.Sprintf("https://api.fluxer.app/dl/macos/universal/app_tar_gz?channel=%s", *channel),
				"signature": macSig,
			},
			"linux-x86_64": map[string]string{
				"url":       fmt.Sprintf("https://api.fluxer.app/dl/linux/x64/appimage?channel=%s", *channel),
				"signature": linX64Sig,
			},
			"linux-aarch64": map[string]string{
				"url":       fmt.Sprintf("https://api.fluxer.app/dl/linux/arm64/appimage?channel=%s", *channel),
				"signature": linArmSig,
			},
		},
	}

	buf, err := json.MarshalIndent(latest, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp := filepath.Join(os.TempDir(), "latest.json")
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}

	return uploader(tmp, fmt.Sprintf("desktop/%s/latest.json", *channel), "application/json")
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

func readSig(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(bytes.ReplaceAll(data, []byte("\n"), nil))), nil
}
