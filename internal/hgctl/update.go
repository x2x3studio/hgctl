package hgctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	updateInterval           = 1 * time.Hour
	updateCheckSchemaVersion = 1
	maxCandidateVersionBytes = 4 * 1024
)

// GitHub's REST API rejects requests without a User-Agent.
const releaseUserAgent = "hgctl-updater"

// releaseLatestURL is the unauthenticated GitHub REST endpoint for the latest
// hgctl release. It is a variable so tests can point it at a local server.
var releaseLatestURL = "https://api.github.com/repos/x2x3studio/hgctl/releases/latest"

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
	URL  string `json:"browser_download_url"`
}

type updateCheck struct {
	SchemaVersion int       `json:"schema_version"`
	CheckedAt     time.Time `json:"checked_at"`
}

func (a *App) update(ctx context.Context, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	updateLocked := func() error {
		previous, err := a.loadUpdateCheck()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		now := a.Now().UTC()
		if !force && !previous.CheckedAt.IsZero() && now.Sub(previous.CheckedAt) < updateInterval {
			return nil
		}
		if err := a.saveUpdateCheck(updateCheck{CheckedAt: now}); err != nil {
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
		if err := a.installReleaseBinary(ctx, rel.TagName, binary); err != nil {
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

func (a *App) loadUpdateCheck() (updateCheck, error) {
	var check updateCheck
	if err := readJSON(a.Paths.UpdateCheck, &check); err != nil {
		return updateCheck{}, err
	}
	migrated, err := migrateSchemaVersion(a.Paths.UpdateCheck, &check.SchemaVersion, updateCheckSchemaVersion)
	if err != nil {
		return updateCheck{}, err
	}
	if migrated {
		if err := writeJSONAtomic(a.Paths.UpdateCheck, check, 0o600); err != nil {
			return updateCheck{}, err
		}
	}
	return check, nil
}

func (a *App) saveUpdateCheck(check updateCheck) error {
	check.SchemaVersion = updateCheckSchemaVersion
	return writeJSONAtomic(a.Paths.UpdateCheck, check, 0o600)
}

func (a *App) installReleaseBinary(ctx context.Context, tag string, content []byte) error {
	if _, err := parseSemanticVersion(tag); err != nil {
		return fmt.Errorf("release tag: %w", err)
	}
	version := strings.TrimPrefix(tag, "v")
	targetDir := filepath.Join(a.Paths.Versions, version)
	target := filepath.Join(targetDir, "hgctl")
	link := filepath.Join(a.Paths.Bin, "hgctl")
	previousTarget := ""
	hadPreviousTarget := false
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to replace non-symlink %s", link)
		}
		if !managedStableSymlink(link, a.Paths.Versions) {
			return fmt.Errorf("refusing to replace unmanaged symlink %s", link)
		}
		previousTarget, err = resolvedSymlinkTarget(link)
		if err != nil {
			return err
		}
		hadPreviousTarget = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if hadPreviousTarget && filepath.Clean(previousTarget) == filepath.Clean(target) {
		matches, err := installedBinaryMatches(target, content)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("release target %s already exists with different content", target)
		}
		if err := a.verifyCandidateVersion(ctx, target, tag); err != nil {
			return err
		}
		return nil
	}

	candidate, err := writeExecutableTemp(targetDir, content)
	if err != nil {
		return err
	}
	defer os.Remove(candidate)
	if err := a.verifyCandidateVersion(ctx, candidate, tag); err != nil {
		return err
	}
	if err := os.Rename(candidate, target); err != nil {
		return err
	}
	if err := os.MkdirAll(a.Paths.Bin, 0o755); err != nil {
		return err
	}
	// Cleanup happens before the atomic switch, so every possible failure still
	// leaves the currently running stable binary selected.
	if err := pruneManagedVersions(a.Paths.Versions, target, previousTarget); err != nil {
		return err
	}
	return replaceStableSymlink(link, target)
}

func installedBinaryMatches(path string, expected []byte) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxReleaseBinaryBytes || info.Size() != int64(len(expected)) {
		return false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	want := sha256.Sum256(expected)
	got := sha256.Sum256(content)
	return want == got, nil
}

func writeExecutableTemp(dir string, content []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".hgctl-candidate-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o755); err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

func (a *App) verifyCandidateVersion(parent context.Context, path, tag string) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Env = os.Environ()
	for key, value := range map[string]string{
		"HGCTL_HOME":          a.Paths.Home,
		"HGCTL_DATA_DIR":      a.Paths.Data,
		"HOURGLASS_VAULT":     a.Paths.Vault,
		stateProbeEnvironment: "1",
	} {
		cmd.Env = setEnvironment(cmd.Env, key, value)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := boundedCommandOutput{limit: 64 << 10}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start release candidate: %w", err)
	}
	output, readErr := readLimited(stdout, maxCandidateVersionBytes)
	if readErr != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return fmt.Errorf("read release candidate version: %w", readErr)
	}
	if stderr.truncated {
		return errors.New("release candidate compatibility probe exceeded its stderr limit")
	}
	if waitErr != nil {
		return fmt.Errorf("run release candidate compatibility probe: %w (output suppressed)", waitErr)
	}
	want := stateProbeMarker + "\n" + tag + "\n"
	if string(output) != want {
		return fmt.Errorf("release candidate did not return the compatibility marker and expected exact tag %q", tag)
	}
	return nil
}

func setEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func resolvedSymlinkTarget(link string) (string, error) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return filepath.Clean(target), nil
}

func pruneManagedVersions(root string, current, rollback string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := map[string]bool{filepath.Clean(filepath.Dir(current)): true}
	if rollback != "" {
		keep[filepath.Clean(filepath.Dir(rollback))] = true
	}
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		if keep[filepath.Clean(dir)] || !managedVersionDirectory(entry, dir) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, "hgctl")); err != nil {
			return err
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}

func managedVersionDirectory(entry os.DirEntry, dir string) bool {
	if !entry.IsDir() {
		return false
	}
	name := entry.Name()
	if name != "dev" {
		// installReleaseBinary strips the tag's v prefix. A prefixed directory
		// therefore belongs to another layout and must be preserved.
		if strings.HasPrefix(name, "v") {
			return false
		}
		if _, err := parseSemanticVersion(name); err != nil {
			return false
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "hgctl" {
		return false
	}
	info, err := entries[0].Info()
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func latestRelease(ctx context.Context) (release, error) {
	body, err := httpGetLimited(ctx, releaseLatestURL, "application/vnd.github+json", maxReleaseJSONBytes)
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
	return httpGetLimited(ctx, url, "application/octet-stream", limit)
}

// httpGetLimited fetches url over unauthenticated HTTPS and returns at most
// limit bytes of the body. net/http follows the redirect that release assets
// issue to the CDN by default. The context carries the update deadline.
func httpGetLimited(ctx context.Context, url, accept string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", releaseUserAgent)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %s", url, response.Status)
	}
	return readLimited(response.Body, limit)
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
	major, minor, patch uint64
	prerelease          []string
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
	return compareSemanticVersions(candidateVersion, currentVersion) > 0, nil
}

func parseSemanticVersion(value string) (semanticVersion, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	buildParts := strings.Split(value, "+")
	if len(buildParts) > 2 || (len(buildParts) == 2 && !validSemanticIdentifiers(buildParts[1], false)) {
		return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
	}
	parts := strings.SplitN(buildParts[0], "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) != 3 {
		return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
	}
	parsed := make([]uint64, 3)
	for i, number := range numbers {
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
		}
		parsed[i], _ = strconv.ParseUint(number, 10, 64)
		if strconv.FormatUint(parsed[i], 10) != number {
			return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
		}
	}
	var prerelease []string
	if len(parts) == 2 {
		if !validSemanticIdentifiers(parts[1], true) {
			return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
		}
		prerelease = strings.Split(parts[1], ".")
	}
	return semanticVersion{major: parsed[0], minor: parsed[1], patch: parsed[2], prerelease: prerelease}, nil
}

func validSemanticIdentifiers(value string, prerelease bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" || (prerelease && numericIdentifier(identifier) && len(identifier) > 1 && identifier[0] == '0') {
			return false
		}
		for i := 0; i < len(identifier); i++ {
			character := identifier[i]
			if (character < '0' || character > '9') && (character < 'A' || character > 'Z') &&
				(character < 'a' || character > 'z') && character != '-' {
				return false
			}
		}
	}
	return true
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func compareSemanticVersions(left, right semanticVersion) int {
	for _, pair := range [][2]uint64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(left.prerelease) && index < len(right.prerelease); index++ {
		leftID := left.prerelease[index]
		rightID := right.prerelease[index]
		if leftID == rightID {
			continue
		}
		leftNumeric := numericIdentifier(leftID)
		rightNumeric := numericIdentifier(rightID)
		switch {
		case leftNumeric && rightNumeric:
			if len(leftID) < len(rightID) || (len(leftID) == len(rightID) && leftID < rightID) {
				return -1
			}
			return 1
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case leftID < rightID:
			return -1
		default:
			return 1
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
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
