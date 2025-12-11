# Fluxer Builds

This repository contains CI/CD workflows for building Fluxer desktop and mobile applications.

## Overview

This is a **public repository** used to run builds on GitHub's free CI runners. The actual source code is in a private repository, which is checked out during build using a deploy key.

## Workflows

### Desktop Builds
- **build-desktop-canary.yaml** - Builds desktop app (Windows, macOS, Linux) for canary channel
- **build-desktop-stable.yaml** - Builds desktop app for stable channel

### Mobile Builds
- **build-mobile-android-canary.yaml** - Builds Android app for canary channel
- **build-mobile-android-stable.yaml** - Builds Android app for stable channel
- **build-mobile-ios-canary.yaml** - Builds iOS app for canary channel
- **build-mobile-ios-stable.yaml** - Builds iOS app for stable channel

## How It Works

1. Builds are triggered via `repository_dispatch` events from the private repository
2. Workflows checkout the private source code using a deploy key
3. Builds run on GitHub's free runners
4. Artifacts are uploaded to S3 and GitHub Actions artifacts

## Supported Platforms

| Platform | Architecture | Runner |
|----------|-------------|--------|
| Windows | x64 | `windows-latest` |
| Windows | ARM64 | `windows-11-arm` |
| macOS | x64 | `macos-15-large` |
| macOS | ARM64 | `macos-15-xlarge` |
| Linux | x64 | `ubuntu-latest` |
| Linux | ARM64 | `ubuntu-22.04-arm` |
| Android | - | `ubuntu-latest` |
| iOS | - | `macos-15-xlarge` |

## Required Secrets

The following secrets must be configured in this repository:

### Repository Access
- `FLUXER_PRIVATE_REPO_DEPLOY_KEY` - SSH deploy key for private repo access

### Apple/iOS Signing
- `APPLE_CERTIFICATE`, `APPLE_CERTIFICATE_PASSWORD`
- `APPLE_ID`, `APPLE_PASSWORD`, `APPLE_TEAM_ID`
- `IOS_CERTIFICATE_BASE64`, `IOS_CERTIFICATE_PASSWORD`
- `IOS_PROVISIONING_PROFILE_CANARY_BASE64`, `IOS_PROVISIONING_PROFILE_STABLE_BASE64`
- `KEYCHAIN_PASSWORD`, `MATCH_PASSWORD`
- `APP_STORE_CONNECT_API_KEY_ID`, `APP_STORE_CONNECT_API_ISSUER_ID`, `APP_STORE_CONNECT_API_KEY_CONTENT`

### Android Signing
- `ANDROID_KEYSTORE_BASE64`, `ANDROID_KEYSTORE_PASSWORD`
- `ANDROID_KEY_ALIAS`, `ANDROID_KEY_PASSWORD`
- `GOOGLE_SERVICES_JSON_BASE64`

### Firebase (iOS)
- `GOOGLE_SERVICE_INFO_PLIST_BASE64`

### S3/AWS
- `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`

### Flatpak (Linux)
- `FLATPAK_GPG_PRIVATE_KEY`, `FLATPAK_GPG_PASSPHRASE`
- `FLATPAK_GPG_KEY_ID`, `FLATPAK_GPG_PUBLIC_KEY`

## Triggering Builds

Builds are triggered from the private repository using dispatcher workflows. Do not trigger workflows directly in this repository.
