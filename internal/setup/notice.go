package setup

import (
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/hostconf"
)

// The notice address is root's, not the seedbox's: bootstrap writes the file, mode
// 0600 because the address can carry a token, and init is the one run that asks for
// it -- last, masked, optional, and written with the rest of the answers.

// readNotice is root's config, read strictly like every other run reads it: an
// unknown key, a malformed line or a bad value stops init before its first
// question, naming file, line and key. A typo a run asked around would quietly stop
// updating.
func readNotice(path string) (hostconf.Config, bool, error) {
	if strings.TrimSpace(path) == "" {
		return hostconf.Defaults(), false, nil
	}
	return hostconf.Load(path)
}

// noticePending reports whether root's file exists and has no address.
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

// askNoticeURL is the one optional question: a URL, or Enter to skip. A Secret
// because the address can carry a token, so it is never echoed or printed back.
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

// writeNoticeURL splices the answer into root's file, every other byte kept. What
// box says afterwards is the host, never the address.
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
