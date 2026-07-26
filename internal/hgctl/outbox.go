package hgctl

import (
	"github.com/x2x3studio/hgctl/internal/fsx"
	"path/filepath"
)

// enqueue atomically writes one raw event to the outbox. The thin protocol has
// no content identity or delivery receipt; dedup is provided downstream by git
// (the outbox->queue move plus os.Remove after a successful push).
func (a *App) enqueue(e rawEvent) error {
	return fsx.WriteAtomic(filepath.Join(a.Paths.Outbox, e.filename()), e.marshal(), 0o600)
}
