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

// noticePending reports whether this run has an address to ask for: root's file
// exists, and it has none. A file box cannot read is not "missing" -- it is
// root's, and a question cannot fix it, so its own error stops the run later
// when the file is read.
func (d *Decision) noticePending(opts Options) bool {
	if strings.TrimSpace(opts.NoticePath) == "" {
		return false
	}
	conf, exists, err := hostconf.Load(opts.NoticePath)
	if err != nil || !exists {
		return false
	}
	return conf.NoticeURL == ""
}

// askNoticeURL is the one optional question: a URL, or Enter to skip. It is a
// Secret because the address can carry the token that makes it work, so it is
// never echoed and never printed back.
func (d *Decision) askNoticeURL() (string, error) {
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
