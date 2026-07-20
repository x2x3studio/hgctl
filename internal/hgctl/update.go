package hgctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const updateInterval = 5 * time.Minute

const (
	maxReleaseBinaryBytes = 64 * 1024 * 1024
	maxChecksumBytes      = 2 * 1024 * 1024
	maxReleaseJSONBytes   = 2 * 1024 * 1024
)

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type updateCheck struct {
	CheckedAt time.Time `json:"checked_at"`
}

func (a *App) update(ctx context.Context, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	updateLocked := func() error {
		var previous updateCheck
		if err := readJSON(a.Paths.UpdateCheck, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		now := a.Now().UTC()
		if !force && !previous.CheckedAt.IsZero() && now.Sub(previous.CheckedAt) < updateInterval {
			return nil
		}
		if err := writeJSONAtomic(a.Paths.UpdateCheck, updateCheck{CheckedAt: now}, 0o600); err != nil {
			return err
		}

		rel, err := latestRelease(ctx)
		if err != nil {
			return err
		}
		if rel.TagName == "" {
			return nil
		}
		newer, err := versionIsNewer(Version, rel.TagName)
		if err != nil {
			return err
		}
		if !newer {
			return nil
		}
		binaryName := executableAssetName()
		var binaryAsset, checksumAsset releaseAsset
		for _, asset := range rel.Assets {
			switch asset.Name {
			case binaryName:
				binaryAsset = asset
			case "checksums.txt":
				checksumAsset = asset
			}
		}
		if binaryAsset.URL == "" || checksumAsset.URL == "" {
			return fmt.Errorf("release %s has no %s or checksums.txt", rel.TagName, binaryName)
		}
		binary, err := downloadAsset(ctx, binaryAsset.URL, maxReleaseBinaryBytes)
		if err != nil {
			return err
		}
		checksums, err := downloadAsset(ctx, checksumAsset.URL, maxChecksumBytes)
		if err != nil {
			return err
		}
		expected, err := checksumFor(string(checksums), binaryName)
		if err != nil {
			return err
		}
		actualSum := sha256.Sum256(binary)
		actual := hex.EncodeToString(actualSum[:])
		if !strings.EqualFold(actual, expected) {
			return fmt.Errorf("checksum mismatch for %s", binaryName)
		}
		version := strings.TrimPrefix(rel.TagName, "v")
		target := filepath.Join(a.Paths.Versions, version, "hgctl")
		if err := writeFileAtomic(target, binary, 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(a.Paths.Bin, 0o755); err != nil {
			return err
		}
		link := filepath.Join(a.Paths.Bin, "hgctl")
		if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink != 0 && !managedStableSymlink(link, a.Paths.Versions) {
			return fmt.Errorf("refusing to replace unmanaged symlink %s", link)
		}
		if err := replaceStableSymlink(link, target); err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.Out, "updated hgctl to %s\n", rel.TagName)
		return err
	}
	if force {
		return withFileLockWait(ctx, a.Paths.UpdateLock, updateLocked)
	}
	return withFileLock(a.Paths.UpdateLock, updateLocked)
}

func latestRelease(ctx context.Context) (release, error) {
	if !commandExists("gh") {
		return release{}, errors.New("private release update requires authenticated gh")
	}
	body, err := ghAPI(ctx, "application/vnd.github+json", "repos/x2x3studio/hgctl/releases/latest", maxReleaseJSONBytes)
	if err != nil {
		return release{}, err
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return release{}, err
	}
	return rel, nil
}

func downloadAsset(ctx context.Context, url string, limit int64) ([]byte, error) {
	return ghAPI(ctx, "application/octet-stream", url, limit)
}

func ghAPI(ctx context.Context, accept, endpoint string, limit int64) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "-H", "Accept: "+accept, endpoint)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	body, readErr := readLimited(stdout, limit)
	if readErr != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("gh api: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return body, nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("download exceeds %d bytes", limit)
	}
	return body, nil
}

type semanticVersion struct {
	major, minor, patch int
	prerelease          string
}

func versionIsNewer(current, candidate string) (bool, error) {
	if strings.TrimPrefix(current, "v") == "dev" {
		_, err := parseSemanticVersion(candidate)
		return err == nil, err
	}
	currentVersion, err := parseSemanticVersion(current)
	if err != nil {
		return false, fmt.Errorf("current version: %w", err)
	}
	candidateVersion, err := parseSemanticVersion(candidate)
	if err != nil {
		return false, fmt.Errorf("release version: %w", err)
	}
	left := []int{candidateVersion.major, candidateVersion.minor, candidateVersion.patch}
	right := []int{currentVersion.major, currentVersion.minor, currentVersion.patch}
	for i := range left {
		if left[i] != right[i] {
			return left[i] > right[i], nil
		}
	}
	return currentVersion.prerelease != "" && candidateVersion.prerelease == "", nil
}

func parseSemanticVersion(value string) (semanticVersion, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	value = strings.SplitN(value, "+", 2)[0]
	parts := strings.SplitN(value, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) != 3 {
		return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
	}
	parsed := make([]int, 3)
	for i, number := range numbers {
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
		}
		parsed[i], _ = strconv.Atoi(number)
		if strconv.Itoa(parsed[i]) != number {
			return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
		}
	}
	prerelease := ""
	if len(parts) == 2 {
		prerelease = parts[1]
		if prerelease == "" {
			return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
		}
	}
	return semanticVersion{major: parsed[0], minor: parsed[1], patch: parsed[2], prerelease: prerelease}, nil
}

func checksumFor(content, name string) (string, error) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		filename := strings.TrimPrefix(fields[len(fields)-1], "*")
		if filename == name {
			if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != 64 {
				return "", fmt.Errorf("invalid checksum for %s", name)
			}
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}
