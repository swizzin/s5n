package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/theme"
)

// Options is one box UI: where its terminal is, and whether colour is allowed.
type Options struct {
	// In is the terminal to read keys from, Out the one to draw on. Both must
	// be terminals: Decide is what guarantees that.
	In  io.Reader
	Out io.Writer
	// NoColor draws the same layout with every colour dropped (NO_COLOR).
	NoColor bool
	// Width is how many columns the terminal has, as cmd/box measured it. A
	// zero (a caller that did not measure) is the narrowest width init
	// supports, so text still wraps rather than running off the screen.
	Width int
	// Theme names the palette to draw with: a pair ("s5n", "catppuccin")
	// follows the terminal's background, a variant ("s5n-light",
	// "catppuccin-mocha") pins one, and empty is s5n. theme.Parse is the
	// authority on the names; a name it does not know is reported by Err.
	Theme string
}

// UI is setup.Prompter on huh.
//
// One small form per call, deliberately: init's questions depend on earlier
// answers, so the flow is the wizard and huh only draws one field at a time.
// That also keeps the transcript: each answered form stays on screen above the
// next masthead, which is what makes a run reviewable by scrolling back.
type UI struct {
	in     io.Reader
	out    io.Writer
	styles *huh.Styles
	keys   *huh.KeyMap
	chrome chrome
	// width is the terminal's columns. Everything box draws outside huh's own
	// form wraps to it; see wrapText.
	width int

	// accessible is huh's line mode, driven by a scripted reader in tests. box
	// never selects it: without a terminal huh's own secret field returns an
	// empty string without an error, which is why Decide falls back to
	// setup.LinePrompt instead of huh's accessible mode.
	accessible bool

	// variant is the colours in force. box resolves it once, when the UI is
	// built, so a form and the masthead above it cannot disagree.
	variant theme.Variant
	// themeErr is a theme name that did not resolve. New has no error to
	// return -- cmd/box builds the UI in a function that only hands it back --
	// so the first question asked is where the operator hears about it,
	// rather than a run in colours nobody chose.
	themeErr error
}

// New builds the terminal UI. In and Out default to os.Stdin and os.Stdout.
func New(o Options) *UI {
	in := o.In
	if in == nil {
		in = os.Stdin
	}
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	sel, err := theme.Parse(o.Theme)
	// A pinned variant is not going to use the answer, so the terminal is not
	// asked: the query costs up to lipgloss's two-second timeout on a terminal
	// that does not reply.
	dark := true
	if sel.Variant == nil {
		dark = darkBackground(in, out, o.NoColor)
	}
	variant := sel.Pick(dark)
	use(variant)
	u := &UI{
		in:       in,
		out:      out,
		styles:   huhTheme(o.NoColor),
		keys:     keyMap(),
		width:    o.Width,
		variant:  variant,
		themeErr: err,
	}
	if u.width <= 0 {
		u.width = defaultWidth
	}
	u.chrome.noColor = o.NoColor
	return u
}

// Palette is the variant this UI draws with, resolved and in force.
func (u *UI) Palette() theme.Variant { return u.variant }

// Err is why the UI is not drawing the theme it was asked for, or nil.
func (u *UI) Err() error { return u.themeErr }

// keyMap is box's keys: huh quits on Ctrl-C only, and box's cancel gesture is
// Esc anywhere too, the way every other command's is.
func keyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"))
	return km
}

// cancel maps huh's abort to setup's. init prints its own voice for it
// ("oh heck, nvm") exactly once, so nothing here writes a word.
func cancel(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return setup.ErrCancelled
	}
	return err
}

// Step is setup.Stepper: the flow tells the masthead where it is.
func (u *UI) Step(n, total int, question string) {
	u.chrome.step, u.chrome.total, u.chrome.question = n, total, question
}

// begin prints the chrome above a field and leaves one blank line, so the form
// is drawn under the masthead rather than against it. A step asked in several
// fields shows its masthead once: the fields and the path report under it are
// all the same step.
func (u *UI) begin() {
	if head, ok := u.chrome.head(); ok {
		fmt.Fprintln(u.out, head)
		fmt.Fprintln(u.out)
	}
}

// end separates one field's output from the next masthead.
func (u *UI) end() {
	fmt.Fprintln(u.out)
}

// run draws one field as a whole (one-field) form and waits for the answer.
// answered is called only when the field was answered, and before the blank
// line that separates it from what comes next, so the record it leaves reads
// as part of the field rather than as a stray line.
func (u *UI) run(field huh.Field, answered func()) error {
	if u.themeErr != nil {
		return u.themeErr
	}
	form := huh.NewForm(huh.NewGroup(field)).
		WithTheme(huh.ThemeFunc(func(bool) *huh.Styles { return u.styles })).
		WithKeyMap(u.keys).
		WithShowHelp(false).
		WithInput(u.in).
		WithOutput(u.out)
	if u.accessible {
		form = form.WithAccessible(true)
	}
	u.begin()
	err := cancel(form.Run())
	if err == nil && answered != nil {
		answered()
	}
	u.end()
	return err
}

// record is the one line a finished form leaves in the scrollback: what was
// asked, then what was answered. huh clears its own drawing when the form
// ends, and without this the transcript would keep each question's title and
// drop the answer, which is exactly the part a run is reviewed for.
//
// The value is the answer as a person would say it (an option's label, not the
// value a file keeps), and a secret is recorded as hidden. NO_COLOR keeps the
// same words with no escapes: the check is a glyph, not a colour.
func (u *UI) record(label, value string) {
	label = strings.TrimSpace(label)
	value = strings.TrimSpace(value)
	if label == "" || value == "" {
		return
	}
	mark := u.chrome.paint("✓", accentStyle.Render("✓"))
	title := u.chrome.paint(label, mutedStyle.Render(label))
	head := "  " + mark + " " + title + "  "
	cont := strings.Repeat(" ", lipgloss.Width(head))
	for i, line := range wrapText(value, u.width-lipgloss.Width(head)) {
		body := u.chrome.paint(line, inkStyle.Render(line))
		if i == 0 {
			fmt.Fprintf(u.out, "%s%s\n", head, body)
			continue
		}
		fmt.Fprintf(u.out, "%s%s\n", cont, body)
	}
}

// hidden is what the scrollback says in place of a secret's value.
const hidden = "•••• (hidden)"

// recordSecret is a secret's record line. The value is not an argument, so no
// caller can pass it here by mistake: a secret never reaches the scrollback.
func (u *UI) recordSecret(label string) {
	u.record(label, hidden)
}

// optionLabel is the word a person chose, so a record does not show the value
// the file stores when the two differ.
func optionLabel(options []setup.Option, value string) string {
	for _, o := range options {
		if o.Value == value {
			return o.Label
		}
	}
	return value
}

// optionLabels is optionLabel for a multi-select, in the order the answer
// holds them.
func optionLabels(options []setup.Option, values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, optionLabel(options, v))
	}
	return out
}

// label is what a field is called: the flow's prompt, or the values key when
// the question has no label of its own.
func label(prompt, key string) string {
	if strings.TrimSpace(prompt) != "" {
		return prompt
	}
	return key
}

// Input asks for one line of text.
func (u *UI) Input(f setup.Input) (string, error) {
	value := f.Default
	field := huh.NewInput().
		Key(f.Key).
		Title(label(f.Prompt, f.Key)).
		Description(f.Help).
		Prompt("").
		Value(&value)
	if f.Validate != nil {
		validate := f.Validate
		// The line prompt trims an answer before it validates it, so a stray
		// space cannot decide whether a domain is valid.
		field = field.Validate(func(s string) error { return validate(strings.TrimSpace(s)) })
	}
	if err := u.run(field, func() {
		value = strings.TrimSpace(value)
		u.record(label(f.Prompt, f.Key), value)
		u.chrome.remember(value)
	}); err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	return value, nil
}

// Secret asks for a value that must not appear on screen. Nothing about it is
// remembered for the breadcrumb, and huh masks it with its own echo mode.
func (u *UI) Secret(f setup.Secret) (string, error) {
	var value string
	field := huh.NewInput().
		Key(f.Key).
		Title(label(f.Prompt, f.Key)).
		Description(f.Help).
		Prompt("").
		EchoMode(huh.EchoModePassword).
		Value(&value)
	if !f.Optional {
		field = field.Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("a value is needed")
			}
			return nil
		})
	}
	if err := u.run(field, func() {
		u.recordSecret(label(f.Prompt, f.Key))
	}); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// Confirm asks yes or no. The focused button is the answer to enter, so a
// question whose safe answer is no starts on no.
func (u *UI) Confirm(f setup.Confirm) (bool, error) {
	value := f.Default
	affirmative := f.Affirmative
	if affirmative == "" {
		affirmative = "yes"
	}
	negative := f.Negative
	if negative == "" {
		negative = "no"
	}
	field := huh.NewConfirm().
		Key("confirm").
		Title(f.Prompt).
		Affirmative(affirmative).
		Negative(negative).
		Value(&value)
	if err := u.run(field, func() {
		answer := negative
		if value {
			answer = affirmative
		}
		u.record(label(f.Prompt, "confirm"), answer)
	}); err != nil {
		return false, err
	}
	return value, nil
}

// Select picks exactly one option. huh matches values by comparison, so the
// answer is the option's value and never its label.
func (u *UI) Select(f setup.Select) (string, error) {
	value := f.Default
	if value == "" && len(f.Options) > 0 {
		value = f.Options[0].Value
	}
	field := huh.NewSelect[string]().
		Key(f.Key).
		Title(label(f.Prompt, f.Key)).
		Options(optionKeys(f.Options)...).
		Value(&value)
	if err := u.run(field, func() {
		u.record(label(f.Prompt, f.Key), optionLabel(f.Options, value))
	}); err != nil {
		return "", err
	}
	return value, nil
}

// MultiSelect picks any number of options. Selected is what a re-run shows
// preselected, and Min is the flow's rule, shown in the form rather than
// decided here.
func (u *UI) MultiSelect(f setup.MultiSelect) ([]string, error) {
	keys := optionKeys(f.Options)
	picked := map[string]bool{}
	for _, v := range f.Selected {
		picked[v] = true
	}
	for i := range keys {
		keys[i] = keys[i].Selected(picked[keys[i].Value])
	}
	value := append([]string(nil), f.Selected...)
	field := huh.NewMultiSelect[string]().
		Key(f.Key).
		Title("").
		Description("space toggles · ctrl+a all/none · enter continues").
		Height(len(keys)).
		Options(keys...).
		Value(&value)
	if f.Min > 0 {
		field = field.Validate(func(v []string) error { return tooFew(f.Min, len(v)) })
	}
	if err := u.run(field, func() {
		// The field's own title is empty (the masthead carries the question),
		// so the record names it by its key: "apps  qbittorrent, sonarr".
		name := f.Key
		if strings.TrimSpace(name) == "" {
			name = f.Prompt
		}
		u.record(name, strings.Join(optionLabels(f.Options, value), ", "))
	}); err != nil {
		return nil, err
	}
	return value, nil
}

// Note shows a block of text without asking anything. init sends the path
// report through ShowPathCheck instead; Note is the general seam, kept so this
// type is a complete Prompter.
func (u *UI) Note(text string) error {
	if u.themeErr != nil {
		return u.themeErr
	}
	u.begin()
	fmt.Fprintln(u.out, u.chrome.paint(text, inkStyle.Render(text)))
	u.end()
	return nil
}

// optionKeys maps setup's options onto huh's: the label, padded so a column of
// help reads as a column, then the help in the same row.
func optionKeys(options []setup.Option) []huh.Option[string] {
	width := 0
	for _, o := range options {
		if n := len([]rune(o.Label)); n > width {
			width = n
		}
	}
	out := make([]huh.Option[string], 0, len(options))
	for _, o := range options {
		text := o.Label
		if o.Help != "" {
			text = fmt.Sprintf("%-*s  %s", width, o.Label, o.Help)
		}
		out = append(out, huh.NewOption(text, o.Value))
	}
	return out
}

// tooFew is the multi-select's rule: the same words the line prompt uses, so a
// person sees the same sentence whichever UI they got.
func tooFew(min, got int) error {
	if got >= min {
		return nil
	}
	return fmt.Errorf("at least %d %s needed", min, plural(min, "app", "apps"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

var (
	_ setup.Prompter    = (*UI)(nil)
	_ setup.Stepper     = (*UI)(nil)
	_ setup.PathChecker = (*UI)(nil)
)
