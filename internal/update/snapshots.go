package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/snapshot"
)

// ConfigSnapshots is the Snapshotter over a seedbox's config root: the copy
// internal/snapshot takes, arranged by the box's own layout.
//
// A step's apps are the values' app keys. Each one owns a directory per enabled
// instance under the config root -- `<root>/sonarr`, `<root>/sonarr-4k` -- because
// that is the subPath each pod mounts (setup.SettingsDirs names the same list
// init creates), and what is copied and restored is the files the release's
// catalog names inside each of those directories. So an app is covered only
// when every instance of it is: a half-copied app would restore one instance's
// database over another's config, or leave one on the failed image's schema.
type ConfigSnapshots struct {
	// ConfigRoot is the grant's storage.config.path: the host directory each
	// instance's config lives under.
	ConfigRoot string
	// Values are the effective values this run planned with, which name the
	// enabled instances.
	Values map[string]any
	// Globs is one release's catalog, app -> the files an image upgrade can
	// rewrite, for the release the step installs. It is a function because
	// each step installs a different release.
	Globs func(ctx context.Context, version string) (map[string][]string, error)
	// Now is time.Now, so a test can pin the step directory's timestamp.
	Now func() time.Time

	// taken is what each step's Take wrote, which Restore reads: a rollback
	// is always the same run that copied, and the directory's timestamp is
	// the one Take chose rather than a second clock read.
	taken map[Step]Taken
}

// Check refuses a covered app whose config directory is missing. It reads
// nothing else: a glob that matches no file is Take's refusal, not this one, and
// checking it here would stat an app twice.
func (s *ConfigSnapshots) Check(ctx context.Context, _ Step, apps []string) error {
	for _, app := range apps {
		for _, dir := range s.dirs(app) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := os.Stat(dir); err != nil {
				return fmt.Errorf("%s: %w", dir, err)
			}
		}
	}
	return nil
}

// Take copies each app's instances into one step directory and returns what it
// wrote. Every instance of one step shares the timestamp, so a later rollback
// finds one directory holding every app the step moved.
func (s *ConfigSnapshots) Take(ctx context.Context, step Step, apps []string, images map[string]ImageChange) (Taken, error) {
	globs, err := s.globs(ctx, step)
	if err != nil {
		return Taken{}, err
	}
	at := s.now()
	taken := Taken{At: at, Dirs: make(map[string]string, len(apps))}
	for _, app := range apps {
		appGlobs := globs[app]
		if len(appGlobs) == 0 {
			continue
		}
		change := images[app]
		for _, dir := range s.dirs(app) {
			if err := ctx.Err(); err != nil {
				return Taken{}, err
			}
			dest, err := snapshot.Dir(s.ConfigRoot, step.From, step.To, at, filepath.Base(dir))
			if err != nil {
				return Taken{}, fmt.Errorf("%s: %w", app, err)
			}
			if _, err := snapshot.Take(dir, appGlobs, dest, snapshot.Images{Ran: change.Ran, MovedTo: change.MovedTo}); err != nil {
				return Taken{}, fmt.Errorf("%s: %w", app, err)
			}
			if taken.Step == "" {
				taken.Step = snapshot.StepName(dest)
			}
			if _, ok := taken.Dirs[app]; !ok {
				taken.Dirs[app] = dest
			}
		}
	}
	if taken.Step == "" {
		return Taken{}, fmt.Errorf("snapshot: none of %s has a catalog entry naming files to copy", strings.Join(apps, ", "))
	}
	if s.taken == nil {
		s.taken = make(map[Step]Taken)
	}
	s.taken[step] = taken
	return taken, nil
}

// Restore puts each app's instances back to the copy this step took. It refuses
// a step it did not copy in this run: restoring the newest snapshot instead
// would roll the app back past state it has written since.
func (s *ConfigSnapshots) Restore(ctx context.Context, step Step, apps []string) error {
	taken, ok := s.taken[step]
	if !ok {
		return fmt.Errorf("snapshot: no copy of %s was taken for %s -> %s", strings.Join(apps, ", "), step.From, step.To)
	}
	globs, err := s.globs(ctx, step)
	if err != nil {
		return err
	}
	for _, app := range apps {
		appGlobs := globs[app]
		if len(appGlobs) == 0 {
			continue
		}
		for _, dir := range s.dirs(app) {
			if err := ctx.Err(); err != nil {
				return err
			}
			src, err := snapshot.Dir(s.ConfigRoot, step.From, step.To, taken.At, filepath.Base(dir))
			if err != nil {
				return fmt.Errorf("%s: %w", app, err)
			}
			if err := snapshot.Restore(dir, appGlobs, src); err != nil {
				return fmt.Errorf("%s: %w", app, err)
			}
		}
	}
	return nil
}

// Prune keeps each instance's two newest snapshots. Snapshot.Prune is per
// config directory, so it is one call per instance, which is also what lets the
// directory left empty by a prune go with it.
func (s *ConfigSnapshots) Prune(ctx context.Context, _ Step, apps []string) error {
	for _, app := range apps {
		for _, dir := range s.dirs(app) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := snapshot.Prune(snapshot.Root(s.ConfigRoot), filepath.Base(dir), 2); err != nil {
				return err
			}
		}
	}
	return nil
}

// dirs is one app's config directories: the instances the values enable, named
// as the chart mounts them.
func (s *ConfigSnapshots) dirs(app string) []string {
	return setup.SettingsDirs(s.ConfigRoot, []string{app}, s.Values)
}

// globs is the step's release catalog, read through the one chart source the
// update pulled it from.
func (s *ConfigSnapshots) globs(ctx context.Context, step Step) (map[string][]string, error) {
	if s.Globs == nil {
		return nil, nil
	}
	return s.Globs(ctx, step.To)
}

func (s *ConfigSnapshots) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
