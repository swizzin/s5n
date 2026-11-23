package apply

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// TestSummaryNamesTheAccountItRenames: an app that keeps its own user table gets
// one account, box's, and the confirm is where the operator reads that the name
// they used to sign in with is not the name any more -- and that the app's login
// was never their way in. The source's name is their own data on their own
// screen, so the line prints it.
func TestSummaryNamesTheAccountItRenames(t *testing.T) {
	s := Summary{Instances: []InstanceSummary{{
		App:      "autobrr",
		Accounts: []rewrite.Account{{From: "source-login", To: "s5n"}},
	}}}
	want := `autobrr: account "source-login" becomes s5n, box's own; sign in through the box login`
	if got := strings.Join(s.Lines(), "\n"); got != want {
		t.Errorf("the summary reads:\n%s\nwant:\n%s", got, want)
	}
}

// TestAWriteThatSkipsTheAccountRenameIsRefused: apply holds every Write to the
// Check before it. An account rename is not a counted column change, so it has
// its own comparison: a Write that reported a different set of renamed accounts
// than its Check is a Write that did less (or more) than the operator was shown.
func TestAWriteThatSkipsTheAccountRenameIsRefused(t *testing.T) {
	renamed := rewrite.Report{App: "qui", Accounts: []rewrite.Account{{From: "source-login", To: "s5n"}}}
	if why, same := sameReport(renamed, renamed); !same {
		t.Errorf("a write that made the check's own change was refused: %s", why)
	}
	if _, same := sameReport(renamed, rewrite.Report{App: "qui"}); same {
		t.Error("a write that renamed no account was accepted as the check's")
	}
	different := rewrite.Report{App: "qui", Accounts: []rewrite.Account{{From: "another-login", To: "s5n"}}}
	if _, same := sameReport(renamed, different); same {
		t.Error("a write that renamed a different account was accepted as the check's")
	}
}
