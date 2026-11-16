package setup

import (
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/hostconf"
)

// The notice address is root's, not the seedbox's: its file is written by
// bootstrap, it is mode 0600 because the address can carry a token, and init is
// the one run that asks for it. It is asked last, masked, optional, and written
// with the rest of the answers -- so a cancel before it leaves root's file
// exactly as bootstrap wrote it.

// readNotice is root's config, read strictly. Every run that reads the file
// parses it the same way, and init is no exception: an unknown key, a malformed
// line or a bad value stops the run before its first question, naming the file,
// the line and the key. A typo that a run asked around would be one that quietly
// stopped updating.
func readNotice(path string) (hostconf.Config, bool, error) {
	if strings.TrimSpace(path) == "" {
		return hostconf.Defaults(), false, nil
	}
	return hostconf.Load(path)
}

// noticePending reports whether this run has an address to ask for: root's file
// exists, and it has none.
func (d *Decision) noticePending(opts Options) (bool, error) {
	conf, exists, err := readNotice(opts.NoticePath)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	return conf.NoticeURL == "", nil
}

// askNoticeURL is the one optional question: a URL, or Enter to skip. It is a
// Secret because the address can carry the token that makes it work, so it is
// never echoed and never printed back. ask is false when this run has nothing to
// ask, and the answer is empty then.
func (d *Decision) askNoticeURL(ask bool) (string, error) {
	if !ask {
		return "", nil
	}
	d.step(d.steps, "where should box post notices?")
	answer, err := d.Prompt.Secret(Secret{
		Key:      "notices.url",
		Prompt:   "Where should box post notices? A URL, or Enter to skip",
		Help:     "box POSTs a JSON notice after an update and when an app stops being healthy",
		Optional: true,
	})
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", nil
	}
	if err := hostconf.ValidURL(answer); err != nil {
		return "", err
	}
	return answer, nil
}

// writeNoticeURL splices the answer into root's file: the one url line, every
// other byte kept. What box says about it afterwards is the host, never the
// address.
func (d *Decision) writeNoticeURL(path, url string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := hostconf.SetNoticeURL(data, url)
	if err != nil {
		return err
	}
	if err := hostconf.Write(path, updated); err != nil {
		return err
	}
	if host := hostconf.Redact(url); host != "" {
		d.report().Muted("notices go to " + host)
	}
	return nil
}
