package hgctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *App) installBinary() error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(Version, "v")
	if version == "" || strings.ContainsAny(version, "/\\") {
		version = "dev"
	}
	target := filepath.Join(a.Paths.Versions, version, "hgctl")
	if err := writeFileAtomic(target, content, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(a.Paths.Bin, 0o755); err != nil {
		return err
	}
	link := filepath.Join(a.Paths.Bin, "hgctl")
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink != 0 && !managedStableSymlink(link, a.Paths.Versions) {
		return fmt.Errorf("refusing to replace unmanaged symlink %s", link)
	}
	return replaceStableSymlink(link, target)
}

func replaceStableSymlink(link, target string) error {
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to replace non-symlink %s", link)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := link + ".new"
	if info, err := os.Lstat(tmp); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to replace non-symlink %s", tmp)
		}
		if err := os.Remove(tmp); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func managedStableSymlink(link, versions string) bool {
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	rel, err := filepath.Rel(canonicalPath(versions), canonicalPath(target))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
