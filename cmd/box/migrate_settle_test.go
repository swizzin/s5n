package main

// migrate_settle_test.go drives the part of a read that answers for what the
// checks could not settle: the default an unattended run takes and names, the
// question a guided run asks, and the report file that carries both.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// unsettledSource is discoverySource with one value no rule covers: Sonarr's
// import list writes its shows into a directory outside every mount, which is
// the shape of a path a migration leaves exactly where it is.
func unsettledSource(t *testing.T) (manifests, root string) {
	t.Helper()
	manifests, root = discoverySource(t)
	db, err := sqlitedb.OpenReadWrite(context.Background(), filepath.Join(root, "srv/apps/sonarr/sonarr.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`INSERT INTO ImportLists (Id, Name, Implementation, Settings, RootFolderPath) VALUES (1, 'Elsewhere', 'Synthetic', '{}', '/mnt/other/shows')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return manifests, root
}

// TestMigrateDiscoverTakesAndNamesEveryDefault: with nobody to ask, every
// default Settle took is taken, said out loud with the same lines the report
// carries, and listed again as the last thing the run prints.
func TestMigrateDiscoverTakesAndNamesEveryDefault(t *testing.T) {
	manifests, root := unsettledSource(t)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	var printed bytes.Buffer
	text, err := migrateDiscover(context.Background(), o, &say.Plain{W: &printed})
	if err != nil {
		t.Fatalf("discover: %v\n%s", err, text)
	}
	// report.txt carries the called-out block, grouped by decision, with the
	// reason and the places that hold the value.
	for _, want := range []string{"called out:", "left as it is:", "/mnt/other/shows", "sonarr: sonarr.db ImportLists.RootFolderPath"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not say %q:\n%s", want, text)
		}
	}
	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"))
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	if string(opened.Report) != text {
		t.Error("the snapshot's report differs from the report the command printed")
	}
	// The plan carries the decision as an identity rule, with its reason.
	p, err := plan.Parse(opened.Plan)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	var rules []plan.PathRule
	for _, r := range p.Apps["sonarr"].Paths {
		if r.From == "/mnt/other/shows" {
			rules = append(rules, r)
		}
	}
	if len(rules) != 1 || !rules[0].Default || rules[0].To != "/mnt/other/shows" || rules[0].Why == "" {
		t.Errorf("sonarr's rule for the uncovered path = %+v, want an identity default with a reason", rules)
	}
	// And the run said so, twice: once where it could not settle it, and once
	// as the last thing on the screen.
	lines := printed.String()
	if !strings.Contains(lines, "taking every one of these defaults") {
		t.Errorf("the run did not say it was taking the defaults:\n%s", lines)
	}
	if !strings.Contains(lines, "defaults taken") {
		t.Errorf("the run's last screen does not list the defaults it took:\n%s", lines)
	}
	if got := lines[strings.LastIndex(lines, "defaults taken"):]; !strings.Contains(got, "/mnt/other/shows") {
		t.Errorf("the last screen does not name the path it defaulted:\n%s", got)
	}
}

// fakeSelect is a prompter that answers the defaults, and records what it was
// asked: the guided half of the same run.
type fakeSelect struct {
	answers []string
	asked   []setup.Select
	// no makes every Confirm answer no, and confirms is every one it was asked:
	// a test reads the question and the answer it takes when nothing is typed.
	no       bool
	confirms []setup.Confirm
}

func (f *fakeSelect) Input(setup.Input) (string, error)   { return "", nil }
func (f *fakeSelect) Secret(setup.Secret) (string, error) { return "", nil }
func (f *fakeSelect) Confirm(q setup.Confirm) (bool, error) {
	f.confirms = append(f.confirms, q)
	if f.no {
		return false, nil
	}
	return true, nil
}
func (f *fakeSelect) MultiSelect(setup.MultiSelect) ([]string, error) {
	return nil, nil
}
func (f *fakeSelect) Note(string) error { return nil }

func (f *fakeSelect) Select(q setup.Select) (string, error) {
	f.asked = append(f.asked, q)
	if len(f.answers) == 0 {
		return q.Default, nil
	}
	answer := f.answers[0]
	f.answers = f.answers[1:]
	return answer, nil
}

// TestMigrateDiscoverAsksEachDefault: with somebody to ask, the run asks once
// per address, offers the default first, and writes the answer into the plan as
// the operator's own decision.
func TestMigrateDiscoverAsksEachDefault(t *testing.T) {
	manifests, root := unsettledSource(t)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	o.Prompt = &fakeSelect{}
	var printed bytes.Buffer
	if _, err := migrateDiscover(context.Background(), o, &say.Plain{W: &printed}); err != nil {
		t.Fatalf("discover: %v", err)
	}
	asker, ok := o.Prompt.(*fakeSelect)
	if !ok {
		t.Fatal("the test's prompter is not the one that ran")
	}
	// The fixture has no address nothing settles, so nothing is asked; a path
	// is never a question, because identity is the only answer box could offer.
	if len(asker.asked) != 0 {
		t.Fatalf("asked %d questions about a source with no unsettled address", len(asker.asked))
	}
	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"))
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	p, err := plan.Parse(opened.Plan)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	// The path was not asked about, so it is still a default, and the run's
	// last screen still names it.
	if len(p.Defaults()) != 1 {
		t.Errorf("the plan carries %d defaults, want the path nothing asked about", len(p.Defaults()))
	}
	if !strings.Contains(printed.String(), "defaults taken") {
		t.Errorf("the guided run's last screen does not list what it took:\n%s", printed.String())
	}
}

// TestMigrateDiscoverAsksAboutAnUnsettledAddress: one address no rule settles
// is one question, with box's answer first and the plan's apps as the rest.
func TestMigrateDiscoverAsksAboutAnUnsettledAddress(t *testing.T) {
	manifests, root := unsettledSource(t)
	// PeerBanHelper's downloader points at a public name with a path, which no
	// source object settles; its own type says it can only be the box's
	// qBittorrent, which is the default the run asks about.
	writePbhFixture(t, root)
	manifests = addPbhWorkload(t, manifests)
	out := filepath.Join(t.TempDir(), "source.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	asker := &fakeSelect{answers: []string{"sonarr"}}
	o.Prompt = asker
	if _, err := migrateDiscover(context.Background(), o, &say.Plain{W: &bytes.Buffer{}}); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(asker.asked) != 1 {
		t.Fatalf("asked %d questions, want one for the one unsettled address", len(asker.asked))
	}
	q := asker.asked[0]
	if !strings.Contains(q.Prompt, "example.com") {
		t.Errorf("the question is not about the stored address:\n%s", q.Prompt)
	}
	if q.Default == "" || q.Options[0].Value != q.Default {
		t.Errorf("box's answer is not the select's default: %+v", q)
	}
	if q.Default != "qbittorrent" {
		t.Errorf("box's default for a qBittorrent downloader = %q, want qbittorrent", q.Default)
	}
	// The default is the first option and says so, and leaving the address is
	// always one of the answers.
	if !strings.Contains(q.Options[0].Label, "box's default") {
		t.Errorf("box's answer is not labelled as the default: %+v", q.Options[0])
	}
	var leave bool
	for _, o := range q.Options {
		if o.Value == leaveValue && strings.Contains(o.Label, "leave it as it is") {
			leave = true
		}
	}
	if !leave {
		t.Errorf("the question does not offer leaving the address alone: %+v", q.Options)
	}
	// And the reason is on the question, single-line, rather than a paragraph in
	// the live form's title.
	if strings.Contains(q.Prompt, "\n") || !strings.Contains(q.Prompt, "leave it as it is, or point it at an app?") {
		t.Errorf("the question reads:\n%s", q.Prompt)
	}
	opened, err := bundle.Open(out, filepath.Join(t.TempDir(), "open"))
	if err != nil {
		t.Fatalf("the snapshot does not open: %v", err)
	}
	p, err := plan.Parse(opened.Plan)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	var found *plan.HostRule
	for i := range p.Hosts {
		if p.Hosts[i].Host == "example.com" {
			found = &p.Hosts[i]
		}
	}
	if found == nil {
		t.Fatal("the plan has no rule for example.com")
	}
	if found.App != "sonarr" || found.Default {
		t.Errorf("the answered rule = %+v, want sonarr and the operator's own decision", *found)
	}
}

// writePbhFixture puts PeerBanHelper's config on the source host, pointing its
// downloader at a public name with a path: the address the plan cannot settle
// and the field that says what it can only mean.
func writePbhFixture(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "srv/apps/pbh/config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := `config-version: 48
language: default
server:
  http: 9898
  address: "127.0.0.1"
  prefix: "http://127.0.0.1:9898"
  token: "synthetic-token"
client:
  qbt_main:
    type: qbittorrent
    name: qBittorrent
    endpoint: https://example.com/qbittorrent
    username: admin
    password: synthetic-password
`
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
}

// addPbhWorkload adds one more workload to the saved manifests, with the mount
// the config beside it lives on.
func addPbhWorkload(t *testing.T, manifests string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(manifests, "source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	extra := `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pbh
  namespace: media
spec:
  selector:
    matchLabels:
      app: pbh
  template:
    metadata:
      labels:
        app: pbh
    spec:
      containers:
        - name: pbh
          image: ghcr.io/peerbanhelper/peerbanhelper:9.0.0
          ports:
            - name: http
              containerPort: 9898
          volumeMounts:
            - name: config
              mountPath: /config
      volumes:
        - name: config
          hostPath:
            path: /srv/apps/pbh
`
	if err := os.WriteFile(filepath.Join(manifests, "source.yaml"), append(raw, []byte(extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return manifests
}
