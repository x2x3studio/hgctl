package hgctl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/x2x3studio/hgctl/internal/config"
	"github.com/x2x3studio/hgctl/internal/fsx"
)

// Version is stamped at build time by the release workflow, which builds a
// timestamp version (v0.<YYYYMMDD>.<second-of-day>).
//
// The default is "dev", not a plausible-looking semver. versionIsNewer already
// treats "dev" as "older than any real release", so an unstamped build updates
// itself on the next check instead of claiming to be v0.2.0 - a string that
// looks like a release nobody ever cut, and that a reader comparing two machines
// would take at face value. CI asserts the stamp actually reaches the binary,
// because Go silently ignores an -X whose package path does not resolve.
var Version = "dev"

// App is the CLI's context, not a god object: the paths this machine uses, the
// three streams, and a clock the tests replace. Everything else is behaviour
// living in a package that takes what it needs.
type App struct {
	Paths config.Paths
	In    io.Reader
	Out   io.Writer
	Err   io.Writer
	Now   func() time.Time
}

// New builds the App from the ambient environment.
//
// The probe branch exists for the updater: a downloaded candidate is run with
// ProbeEnv set before it is promoted, so a release that cannot read this
// machine's persisted schema fails HERE, read-only, instead of after it has
// replaced the working binary.
func New(in io.Reader, out, errOut io.Writer) (*App, error) {
	paths, err := config.Default()
	if err != nil {
		return nil, err
	}
	app := &App{Paths: paths, In: in, Out: out, Err: errOut, Now: time.Now}
	if os.Getenv(config.ProbeEnv) == "1" {
		if err := app.probePersistedStateCompatibility(); err != nil {
			return nil, fmt.Errorf("persisted state compatibility: %w", err)
		}
		if _, err := fmt.Fprintln(out, config.ProbeMarker); err != nil {
			return nil, err
		}
	}
	return app, nil
}

// probePersistedStateCompatibility reads every persisted file and writes none.
func (a *App) probePersistedStateCompatibility() error {
	var identity config.Identity
	if exists, err := fsx.ProbeSchema(a.Paths.Identity, &identity, &identity.SchemaVersion, config.IdentitySchemaVersion); err != nil {
		return err
	} else if exists && !config.ValidMachineID(identity.ID) {
		return errors.New("identity.json has an invalid machine id")
	}
	var state config.State
	if _, err := fsx.ProbeSchema(a.Paths.State, &state, &state.SchemaVersion, config.StateSchemaVersion); err != nil {
		return err
	}
	var receipt config.IndexReceipt
	if _, err := fsx.ProbeSchema(a.Paths.IndexedSHA, &receipt, &receipt.SchemaVersion, config.IndexReceiptSchemaVersion); err != nil {
		return err
	}
	var check updateCheck
	if _, err := fsx.ProbeSchema(a.Paths.UpdateCheck, &check, &check.SchemaVersion, updateCheckSchemaVersion); err != nil {
		return err
	}
	return nil
}
