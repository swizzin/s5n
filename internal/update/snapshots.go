package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/snapshot"
)

// ConfigSnapshots copies each app's enabled instances under the config root --
// the subPath each pod mounts, as setup.SettingsDirs names it -- and restores
// the files the release's catalog names. An app must be covered in every
// instance: a half copy could restore one instance's database over another's.
type ConfigSnapshots struct {
	// ConfigRoot is the grant's storage.config.path.
	ConfigRoot string
	// Values are this run's effective values, which name the enabled instances.
	Values map[string]any
	// Globs is the catalog of the release the step installs; a function because
	// each step installs a different one.
	Globs func(ctx context.Context, version string) (map[string][]string, error)
	// Now is time.Now, so a test can pin the step directory's timestamp.
	Now func() time.Time

	// taken is what each step's Take wrote: Restore is always the same run, and
	// it reuses Take's timestamp rather than reading the clock again.
	taken map[Step]Taken
}

// Check refuses a covered app whose config directory is missing; a glob that
// matches no file is Take's refusal.
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

// Take copies each app's instances into one step directory, all sharing a
// timestamp so a rollback finds every app together.
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

// Restore puts each app's instances back to this step's copy. It refuses a step
// it did not copy: the newest snapshot would be past state written since.
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

// Prune keeps each instance's two newest snapshots; one call per instance, so
// an emptied directory goes with it.
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

// dirs is one app's config directories, as the chart mounts them.
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

// NewestSnapshots reads the newest copy of each of an app's instances, which is
// what a hand rollback asks for; a step instead restores only its own copy. An
// app is covered only when every instance is.
type NewestSnapshots struct {
	// ConfigRoot is the grant's storage.config.path.
	ConfigRoot string
	// Values are this run's effective values, which name the enabled instances.
	Values map[string]any
}

// Newest reads every enabled instance's newest snapshot. An instance never
// copied comes back with an empty Dir; refusing it is the caller's question.
func (s *NewestSnapshots) Newest(ctx context.Context, app string) (AppSnapshot, error) {
	out := AppSnapshot{App: app}
	for _, dir := range setup.SettingsDirs(s.ConfigRoot, []string{app}, s.Values) {
		if err := ctx.Err(); err != nil {
			return AppSnapshot{}, err
		}
		inst := InstanceSnapshot{Name: filepath.Base(dir)}
		src, man, err := snapshot.Newest(snapshot.Root(s.ConfigRoot), inst.Name)
		switch {
		case errors.Is(err, snapshot.ErrNoSnapshot):
			out.Instances = append(out.Instances, inst)
			continue
		case err != nil:
			return AppSnapshot{}, err
		}
		inst.Dir, inst.Step, inst.Manifest = src, snapshot.StepName(src), man
		out.Instances = append(out.Instances, inst)
	}
	return out, nil
}

// Restore puts each instance back one file at a time, so a crash leaves each
// file either old or new.
func (s *NewestSnapshots) Restore(ctx context.Context, snap AppSnapshot, globs []string) error {
	for _, inst := range snap.Instances {
		if inst.Dir == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Its own copy's globs decide which files a restore may remove.
		instGlobs := inst.Manifest.Globs
		if len(instGlobs) == 0 {
			instGlobs = globs
		}
		if err := snapshot.Restore(filepath.Join(s.ConfigRoot, inst.Name), instGlobs, inst.Dir); err != nil {
			return fmt.Errorf("%s: %w", inst.Name, err)
		}
	}
	return nil
}
