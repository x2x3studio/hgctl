package hgctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/x2x3studio/hgctl/internal/fsx"
)

// Backport basicmachines-co/basic-memory@1ad049c704a51ca5477b2cdb814cdc0121578f82.
// Version 0.23.2 correlates the DELETE with every vector row, making a no-op
// reconciliation quadratic. Keep the published runtime and only replace that SQL.
const legacyVectorCleanupSQL = `                    "SELECT id FROM search_vector_chunks "
                    "WHERE project_id = :project_id AND NOT ("
                    "vector_index = 'sqlite-vec' "
                    "AND embedding_model = :embedding_identity "
                    "AND search_vector_embeddings.source_hash = "
                    "search_vector_chunks.source_hash "
                    "AND embedding_status = 'ready'))"`

const fixedVectorCleanupSQL = `                    "SELECT chunks.id FROM search_vector_chunks AS chunks "
                    "JOIN search_vector_embeddings AS vectors ON vectors.rowid = chunks.id "
                    "WHERE chunks.project_id = :project_id AND NOT ("
                    "chunks.vector_index = 'sqlite-vec' "
                    "AND chunks.embedding_model = :embedding_identity "
                    "AND vectors.source_hash = chunks.source_hash "
                    "AND chunks.embedding_status = 'ready'))"`

func (a *App) repairBasicMemoryVectorCleanup(ctx context.Context) (bool, error) {
	binary, err := exec.LookPath("basic-memory")
	if err != nil {
		return false, err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return false, err
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(binary)), "lib", "python*", "site-packages", "basic_memory", "repository", "sqlite_vec_index.py"))
	if err != nil {
		return false, err
	}
	if len(paths) == 0 {
		return false, nil
	}
	if len(paths) != 1 {
		return false, errors.New("ambiguous Basic Memory installation for vector cleanup repair")
	}
	changed := false
	err = fsx.WithLockWait(ctx, filepath.Join(a.Paths.Data, "basic-memory-compat.lock"), func() error {
		var err error
		changed, err = repairVectorCleanupFile(paths[0])
		return err
	})
	if changed {
		_, _ = fmt.Fprintln(a.Err, "hgctl: applied upstream SQLite vector cleanup fix to Basic Memory")
	}
	return changed, err
}

func repairVectorCleanupFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("refusing non-regular Basic Memory source")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if bytes.Contains(content, []byte(fixedVectorCleanupSQL)) {
		return false, nil
	}
	count := bytes.Count(content, []byte(legacyVectorCleanupSQL))
	if count == 0 {
		return false, nil
	}
	if count != 1 {
		return false, errors.New("ambiguous Basic Memory vector cleanup SQL")
	}
	backup := path + ".hgctl-before-vector-cleanup"
	if info, err := os.Lstat(backup); err == nil && !info.Mode().IsRegular() {
		return false, errors.New("refusing non-regular Basic Memory compatibility backup")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if existing, err := os.ReadFile(backup); err == nil {
		if !bytes.Equal(existing, content) {
			return false, errors.New("Basic Memory compatibility backup differs; refusing overwrite")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := fsx.WriteAtomic(backup, content, info.Mode().Perm()); err != nil {
			return false, err
		}
	} else {
		return false, err
	}
	fixed := bytes.Replace(content, []byte(legacyVectorCleanupSQL), []byte(fixedVectorCleanupSQL), 1)
	if err := fsx.WriteAtomic(path, fixed, info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}
