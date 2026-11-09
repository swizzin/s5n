package release

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// tokenKey is the values path the tests treat as transient. It is the real
// chart's: the Plex claim token is the only answer that cannot live in a file.
const tokenKey = "secrets.plexClaimToken"

const token = "claim-SECRET-do-not-print"

type confirmFunc func(string) (bool, error)

func (f confirmFunc) Ask(q string) (bool, error) { return f(q) }

// withTransient is one apply as `box init` runs it: the same files, the same
// chart, plus the hook the claim token comes through.
func (f *fakes) withTransient(t *testing.T, extra map[string]any, events *[]string) (Result, error) {
	t.Helper()
	opts := Options{
		Chart:         testChartPtr(t),
		GrantPath:     f.grantPath,
		ValuesPath:    f.values,
		TransientKeys: []string{tokenKey},
	}
	if extra != nil {
		opts.AfterConfirm = func(context.Context) (map[string]any, error) {
			if events != nil {
				*events = append(*events, "transient")
			}
			return extra, nil
		}
	}
	return f.app.Apply(context.Background(), opts)
}

func snapshot(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = string(data)
	}
	return out
}

func sameFiles(t *testing.T, before map[string]string) {
	t.Helper()
	for path, want := range before {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s changed:\n--- before\n%s\n--- after\n%s", path, want, data)
		}
	}
}

// TestTransientIsAskedAfterTheConfirm: the answer arrives after the operator
// said yes, reaches the release, and is not written to either file.
func TestTransientIsAskedAfterTheConfirm(t *testing.T) {
	f := newFakes(t)
	before := snapshot(t, f.grantPath, f.values)
	var order []string
	f.app.Confirm = confirmFunc(func(string) (bool, error) {
		order = append(order, "confirm")
		return true, nil
	})

	res, err := f.withTransient(t, map[string]any{tokenKey: token}, &order)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.Applied || f.rel.upgrades != 1 {
		t.Fatalf("result = %+v, want one install", res)
	}
	if got := strings.Join(order, ","); got != "confirm,transient" {
		t.Fatalf("order = %s, want the answer asked after the confirm", got)
	}
	if res.Changes != nil {
		t.Fatalf("a first install reported changes: %+v", res.Changes)
	}

	sent := f.rel.upgraded[0]
	secrets, _ := sent["secrets"].(map[string]any)
	if got := secrets[tokenKey[strings.LastIndex(tokenKey, ".")+1:]]; got != token {
		t.Fatalf("the release was not given the answer: %v", sent["secrets"])
	}
	if strings.Contains(f.out.String(), token) {
		t.Fatalf("the answer was printed:\n%s", f.out)
	}
	sameFiles(t, before)
}

// TestTransientIsNeverInALaterDiff: Helm stores what it rendered with, so the
// next run's diff compares the stored answer with the file's empty default.
// That change is dropped, which is what keeps the secret out of the output and
// a re-run a no-op.
func TestTransientIsNeverInALaterDiff(t *testing.T) {
	f := newFakes(t)
	if _, err := f.withTransient(t, map[string]any{tokenKey: token}, nil); err != nil {
		t.Fatalf("install: %v\n%s", err, f.out)
	}
	if got := f.rel.values["secrets"].(map[string]any)["plexClaimToken"]; got != token {
		t.Fatalf("the fake release did not store the answer: %v", f.rel.values["secrets"])
	}
	upgrades := f.rel.upgrades
	f.out.Reset()

	res, err := f.withTransient(t, nil, nil)
	if err != nil {
		t.Fatalf("re-apply: %v\n%s", err, f.out)
	}
	if f.rel.upgrades != upgrades {
		t.Fatalf("the re-run made %d Helm write(s), want 0", f.rel.upgrades-upgrades)
	}
	if res.Applied || len(res.Changes) != 0 {
		t.Fatalf("result = %+v, want nothing to change", res)
	}
	if !strings.Contains(f.out.String(), "nothing to change") {
		t.Fatalf("box did not say nothing to change:\n%s", f.out)
	}
	if strings.Contains(f.out.String(), token) {
		t.Fatalf("the stored answer was printed:\n%s", f.out)
	}
}

// TestTransientCannotSetAGrantKey: the manifest refuses a grant key, but an
// answer is still an answer, and the grant is root's whichever way it arrives.
func TestTransientCannotSetAGrantKey(t *testing.T) {
	f := newFakes(t)
	before := snapshot(t, f.grantPath, f.values)

	_, err := f.withTransient(t, map[string]any{"gateway.host": "other.example.com"}, nil)
	if err == nil || !strings.Contains(err.Error(), "set by the grant") {
		t.Fatalf("error = %v, want a grant-key refusal", err)
	}
	if f.rel.upgrades != 0 || len(f.store.applied) != 0 {
		t.Fatalf("a refusal wrote: upgrades=%d storage=%d", f.rel.upgrades, len(f.store.applied))
	}
	sameFiles(t, before)
}

// TestTransientAskerFailureWritesNothing: a claim token that never arrives stops
// the apply before the storage or the release is written.
func TestTransientAskerFailureWritesNothing(t *testing.T) {
	f := newFakes(t)
	opts := Options{
		Chart:         testChartPtr(t),
		GrantPath:     f.grantPath,
		ValuesPath:    f.values,
		TransientKeys: []string{tokenKey},
		AfterConfirm: func(context.Context) (map[string]any, error) {
			return nil, ErrCancelled
		},
	}
	_, err := f.app.Apply(context.Background(), opts)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if f.rel.upgrades != 0 || len(f.store.applied) != 0 {
		t.Fatalf("a cancelled answer wrote: upgrades=%d storage=%d", f.rel.upgrades, len(f.store.applied))
	}
}

// TestTransientEmptyAnswerIsSkipped: an optional question pressed enter on is
// not a value; the chart sees its own default.
func TestTransientEmptyAnswerIsSkipped(t *testing.T) {
	f := newFakes(t)
	res, err := f.withTransient(t, map[string]any{tokenKey: "  "}, nil)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.Applied {
		t.Fatal("the install did not happen")
	}
	if got := f.rel.upgraded[0]["secrets"].(map[string]any)["plexClaimToken"]; got != "" {
		t.Fatalf("an empty answer became %v", got)
	}
}
