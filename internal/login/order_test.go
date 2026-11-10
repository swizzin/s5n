package login

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// The ordering contract of `box init` is in two places at once: release.Applier
// owns the confirm, and login.Decision is what init runs after it. These tests
// drive both together, with real grant and values files and a fake release
// client, so "a refused confirm writes nothing" and "a login that does not come
// up stops the run before the release" are facts about the code and not about
// the shape of a diagram.

// orderRelease is the Helm client, recording when the release was written.
type orderRelease struct {
	events   *[]string
	upgrades int
}

func (r *orderRelease) Deployed(context.Context) (map[string]any, bool, error) {
	return nil, false, nil
}

func (r *orderRelease) Upgrade(context.Context, release.Chart, map[string]any) (int, error) {
	*r.events = append(*r.events, "release")
	r.upgrades++
	return 1, nil
}

func (r *orderRelease) Receipt(context.Context) (release.Receipt, error) {
	return release.Receipt{Status: "ok", StartedAt: time.Now().Add(time.Minute)}, nil
}

// orderStorage is the claim writer: a fresh cluster, a namespace this identity
// may create, and nothing to find.
type orderStorage struct{}

func (orderStorage) Plan(context.Context, string, []seedbox.Volume) ([]release.StorageObject, error) {
	return nil, nil
}
func (orderStorage) PlanNamespace(context.Context) (release.NamespacePlan, error) {
	return release.NamespacePlan{Name: "seedbox", Exists: true}, nil
}
func (orderStorage) CreateNamespace(context.Context) error              { return nil }
func (orderStorage) Apply(context.Context, release.StorageObject) error { return nil }
func (orderStorage) Delete(context.Context, release.StorageObject) error {
	return nil
}

// answer is the one question apply asks. A plain reporter with Yes=false
// wants a terminal, and a test has none.
type answer bool

func (a answer) Ask(string) (bool, error) { return bool(a), nil }

type orderFixture struct {
	app     *release.Applier
	events  []string
	grant   string
	values  string
	cluster *fakeCluster
	host    *fakeHost
	login   *Decision
}

func newOrderFixture(t *testing.T, confirm bool) *orderFixture {
	t.Helper()
	dir := t.TempDir()
	data, config := filepath.Join(dir, "data"), filepath.Join(dir, "config")
	for _, d := range []string{data, config} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	grant := filepath.Join(dir, "grant.yaml")
	values := filepath.Join(dir, "values.yaml")
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(grant, `namespace: seedbox
login: alice
global:
  puid: 1000
  pgid: 1000
storage:
  data:
    path: `+data+`
  config:
    path: `+config+`
gateway:
  host: box.example.com
apps:
  sonarr:
    enabled: true
`)
	write(values, "tls: {}\n")

	f := &orderFixture{grant: grant, values: values}
	f.cluster = newFakeCluster()
	f.host = newFakeHost(&f.events)
	f.login = &Decision{
		Open:          func() (Cluster, error) { return f.cluster, nil },
		Storage:       func() (release.StorageClient, error) { return orderStorage{}, nil },
		Host:          f.host,
		PolicyTimeout: time.Millisecond,
		Poll:          time.Millisecond,
	}
	rel := &orderRelease{events: &f.events}
	out := &bytes.Buffer{}
	f.app = &release.Applier{
		Confirm:     answer(confirm),
		Say:         &say.Plain{W: out},
		Open:        func(string) (release.ReleaseClient, error) { return rel, nil },
		OpenStorage: func(string) (release.StorageClient, error) { return orderStorage{}, nil },
	}
	return f
}

func (f *orderFixture) apply(t *testing.T) (release.Result, error) {
	t.Helper()
	chart := release.Chart{Values: mustRead(t, "../../charts/s5n/values.yaml"), Schema: mustRead(t, "../../charts/s5n/values.schema.json")}
	opts := testOptions()
	opts.PathsInNode = true // the host write is asserted in login_test.go
	return f.app.Apply(context.Background(), release.Options{
		Chart:      &chart,
		GrantPath:  f.grant,
		ValuesPath: f.values,
		AfterConfirm: func(ctx context.Context) (map[string]any, error) {
			if _, err := f.login.Apply(ctx, opts); err != nil {
				return nil, err
			}
			return nil, nil
		},
	})
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRefusedConfirmWritesNoLogin: the login is asked for after the confirm,
// so a "no" is a run that never touched the cluster or the host.
func TestRefusedConfirmWritesNoLogin(t *testing.T) {
	f := newOrderFixture(t, false)
	_, err := f.apply(t)
	if !errors.Is(err, release.ErrCancelled) {
		t.Fatalf("Apply = %v, want ErrCancelled", err)
	}
	if len(f.cluster.applied) != 0 || len(f.cluster.created) != 0 || len(f.host.files) != 0 {
		t.Fatalf("a refused confirm wrote: %d applied, %d created, %d files", len(f.cluster.applied), len(f.cluster.created), len(f.host.files))
	}
	for _, event := range f.events {
		if event == "release" {
			t.Fatal("a refused confirm installed the release")
		}
	}
}

// TestLoginRunsBeforeTheRelease: Authelia has to be answering and the policy
// accepted before the apps behind the Gateway exist.
func TestLoginRunsBeforeTheRelease(t *testing.T) {
	f := newOrderFixture(t, true)
	if _, err := f.apply(t); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.cluster.applied) == 0 {
		t.Fatal("the login was never applied")
	}
	if len(f.events) == 0 || f.events[len(f.events)-1] != "release" {
		t.Fatalf("events = %v, want the release last", f.events)
	}
}

// TestLoginFailureAbortsBeforeTheRelease: a Deployment that never answers is a
// login nobody can pass, so the release must not be written at all.
func TestLoginFailureAbortsBeforeTheRelease(t *testing.T) {
	f := newOrderFixture(t, true)
	f.cluster.awaitErr = errors.New("the login never answered")
	if _, err := f.apply(t); err == nil {
		t.Fatal("Apply succeeded with a login that never became ready")
	}
	for _, event := range f.events {
		if event == "release" {
			t.Fatalf("the release was installed behind a failed login: %v", f.events)
		}
	}
}
