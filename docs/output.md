# box's terminal output (decided)

This is the style guide every command follows: thirteen rules, each marked
**(decided)**. A renderer is `internal/say` (the plain transcript) and `internal/tui`
(the live one); `internal/runlog` is the transcript file; `box update` is the first
command converted, so its screens at the end are the shape the rest are moving to.

Where a rule and an existing command disagree, the command is the one that has not
been converted yet. New output follows the rules from the day it is written.

## 1. One mark in a two-column gutter (decided)

Every line starts with a mark, a space, and then the line. The marks are:

```
✓  done          ✗  failed        !  needs you      ·  a note
◐  running       ?  a question    $  a command
```

The gutter is two columns wide whatever the glyph costs in bytes, so the columns after
it line up down a whole run. A state is **one short word in its own column** (kept,
new, moved, skipped), never a phrase.

```
✓ snapshots      prowlarr, sonarr · 6s
✓ 0.6.2 → 0.6.3  41s
· nothing on this box changed
```

Not `✓ prowlarr and sonarr were snapshotted and kept as they are`.

## 2. One line, one fact (decided)

No reasoning in the middle of a run. A refusal gets its one reason, and the rest is in
the log.

```
✗ chart          can't read s5n 0.2.0 from the registry
                 it has no platform.yaml
```

Not a paragraph explaining what a chart is and why 0.2.0 predates a file.

## 3. Commands stand alone (decided)

A command is on a line of its own, marked `$`, copyable as it stands. It is never
wrapped in the middle of a token: a long one continues with a trailing ` \`, the way a
shell does. Many similar commands become one.

```
! sonarr is held on 5.0.0.3041 until a release ships a fixed image
  $ box status
```

Not `use box status to check` inside the sentence.

## 4. Success is quiet (decided)

What went to plan folds into a count. A person reading a successful run sees how much
happened, not each thing.

```
✓ wiring         34 checks
```

Not thirty-four lines of `sonarr/downloadclient: ok`.

## 5. Plumbing lives in the log (decided)

Object names, revisions, digests and image ids stay off the screen unless they are the
problem. Every run writes a plain transcript instead, and a failed run's last line
names it.

```
· log            /var/log/box/update-20261123T075500Z.log
```

The transcript is bounded, because a box that checks every fifteen minutes would
otherwise fill a disk: `/var/log/box/<command>-<UTC yyyymmddThhmmssZ>.log` when root,
`$XDG_STATE_HOME/box/` (default `~/.local/state/box/`) otherwise; 0600 files in a 0700
directory; each run prunes the directory to the newest twenty logs and at most 64 MiB,
oldest first, and only ever removes files named the way box names its logs (the
directory `BOX_LOG_DIR` names can hold other programs' logs). The update timer's wakes
log as `update-timer-<UTC>.log` and keep five of those twenty at most, so checks every
fifteen minutes never push out a run a person started. One run's log stops at 16 MiB
with a line saying so. A secret is
registered with the log before it can be written and is replaced where it would have
gone. A log that cannot be written never fails the command: it is one muted line.

## 6. A wait is never silent (decided)

A wait shows one live line: what is happening, how long it has taken against its limit,
and the newest line of the thing being waited on, sampled about once a second and
trimmed to the terminal.

```
◐ waiting for the wiring Job  4m10s of 10m  waiting for pbh to answer
```

Finished, it is one row with its duration. Every wait goes through the same mechanism
(`say.Op` with a `Source`), so adding a log to read costs one argument and nothing else.

```
✓ 0.6.3 → 0.6.4  4m11s
```

The plain transcript does not redraw anything: it prints the finish row, and the log
gets each distinct sampled line once.

## 7. Fits 80 columns (decided)

Text wraps at the terminal's width with continuation lines hanging under the text, never
under the gutter. Tables line up. A list nobody needs in full folds to its size; a list
a person will want to read in full (what a migration moves, renames or leaves behind,
its notes) goes into a sectioned plain-text report beside the snapshot, named in one
line. No ASCII-art panels around paragraphs: a failure is a `✗` row like any other.

```
✗ sonarr         crashed 3 times on the new image
```

## 8. Numbers a person reads (decided)

Sizes, counts and durations are for a person: `491 MiB`, `66,931 paths`, `3m12s`. The
exact byte count and the raw float are elsewhere.

```
✓ plex          1.2 GiB · 4m18s
```

Not `1258291200` or `258.9213s`.

## 9. Headings are labels (decided)

A heading is a lowercase noun, no colon, one level of nesting.

```
platform
  ...
```

Not `=== PLATFORM ===` and not `Steps to perform:`.

## 10. Questions are short (decided)

Context above, as facts. One line of question. The answer collapses to `subject ->
answer`. No annotations telling the reader where their answer goes.

```
s5n · cluster
what domain will this box answer on?

→ box.example.com
```

Not `Please enter the domain name you would like to use (this will also be used for
the certificate) [required]:`.

## 11. Plain voice (decided)

Present tense, second person where a person is addressed. No "box never..." hedging, no
dash or semicolon chains in a sentence. Humour only where it is chosen, and then once.

```
✗ /etc/k0s/k0s.yaml exists
  $ mv /etc/k0s/k0s.yaml /etc/k0s/k0s.yaml.aside
  then run this again
```

Not `box was unable to proceed because — unlike before — the installer's config file
already exists; you may wish to move it.`

## 12. The log looks like the screen (decided)

The transcript is the screen's lines with timestamps, plus the detail lines the screen
leaves out. Reading the log is reading the run in order.

```
2026-11-23T07:55:00Z  s5n 0.6.2 → 0.6.3 · 1 step · prowlarr and sonarr move
2026-11-23T07:55:00Z  
2026-11-23T07:55:06Z  ✓ snapshots      prowlarr, sonarr · 6s
2026-11-23T07:55:06Z  wiring sonarr/downloadclient: ok (updated from 3.0.0)
```

## 13. Data over words (decided)

Values in columns beat phrases that describe them. Less to read, less to translate.

```
/data         xfs     31 GiB
/config       ext4   486 GiB
```

Not `the data and the config are on the same filesystem`.

## The two reference screens

`box update` is the first command converted end to end. A failed run:

```
✓ box            0.6.0 → 0.6.1 · signature ok
✗ chart          can't read s5n 0.2.0 from the registry
                 it has no platform.yaml
· nothing on this box changed
· log            /var/log/box/update-20261123T075500Z.log
```

A step that moved two apps, one of which was put back:

```
s5n 0.6.2 → 0.6.3 · 1 step · prowlarr and sonarr move

✓ snapshots      prowlarr, sonarr · 6s
✓ 0.6.2 → 0.6.3  41s
✗ sonarr         crashed 3 times on the new image
✓ sonarr         rolled back to 5.0.0.3041 · its 15:33 snapshot
✓ wiring         34 checks

! sonarr is held on 5.0.0.3041 until a release ships a fixed image
  $ box status
```

The wiring receipt inside update folds to the one `✓ wiring` row; when something is
pending or failed, those rows are listed instead and the full receipt goes to the log.
`box update --check` and the timer's run follow the same rules.

## The wiring wait and its receipt

Every apply and every update waits for the chart's wiring Job, and the wait has a
log to read: the Job's own. While it waits, one line, with the newest line of that
log beside it.

```
◐ wiring  4m10s of 10m  pbh/ready: waiting at http://pbh:8080
```

The Job's own log goes into the run log, under a `wiring Job log:` heading, with
one line per check, because the screen showed one line at a time and the log is
where the rest is read.

```
✓ wiring         34 checks · 1m2s
```

The count is what the Job checked; the duration is the Job's own. When a check
waits on a person, the count stays and the checks are rows under it -- one mark,
the check's name, its state in the state column, and the reason -- the URL or the
command -- hanging under that. A check that failed is a `✗` row the same way, and
then the count is not printed.

```
✓ wiring         34 checks · 41s
! plex/claim     pending
                 claim it at https://plex.tv/claim
! seerr/plex     pending
                 Seerr has no admin account yet: sign in once at seerr.example.com
```

A box built from a rehearsal snapshot has no internet, so its pending rows are the
real run's to clear. It says so once, under them.

```
· rehearsal · pending rows clear on the real run
```

A Job that never reports is one `✗` row: what box waited for, the Job's newest
line as the reason, and the command that runs it again. The Job's whole log is in
the run log, which the failed run names.

```
✗ wiring         no receipt after 10m
                 pbh/ready: waiting at http://pbh:8080
· your apps are running · only the wiring stopped
  $ box apply
· log            /var/log/box/apply-20261123T055500Z.log
```

## Reference screens: bootstrap, init, apply

`box bootstrap` on a fresh host: the cluster it found in one row, the platform
plan as what this run installs and the one thing it cannot decide yet, then
init's own rows, and one question for both halves. The platform layer and the
seedbox are one decision, so a fresh host is asked once: the platform's plan,
what the seedbox will be (its paths, its host, the torrent port), and then the
answer that installs both, in that order. The kubeconfig path, the four
capability probes, the Helm releases and every Deployment wait are in the log.

```
cluster
✓ kubeadm, 1 node · box uses it as it is
platform
  + cert-manager v1.21.3 · Envoy Gateway 1.9.3 · admission policies
  ? MetalLB 0.16.2, unless the Gateway gets an address without it
where things live
  data    /srv/s5n/data    exists  254:0  xfs  1.8 TiB free
  config  /srv/s5n/config  new     254:0  xfs  1.8 TiB free
· box.example.com  Let's Encrypt · resolves to 203.0.113.7
· torrent port   picked    30013
install the platform and alice's seedbox? [y/N] y
✓ Envoy Gateway  38s
✓ cert-manager   52s
✓ MetalLB        21s
✓ Envoy Gateway  172.18.0.4 · 38s
✓ admission policies  2s
✓ s5n 0.6.3      11 apps · torrent port 30013 · 2m40s
✓ wiring         38 checks
```

A box that already has a platform is not that decision: it asks about each half
of what it will change, and a half with nothing to do asks nothing. `--yes`, or
no terminal, answers without asking, and then the platform's and the release's
confirms are both answered rather than shown.

The two halves install in one order and in one run: the platform layer, then the
seedbox. If the platform is up and the seedbox's install fails, the answers stay
in the two files and the platform stays serving, so the next `box bootstrap`
reads them back, finds the platform, asks only for what changed, and finishes the
same install. Nothing is rolled back for a run that can simply be repeated.

`box init` says where the two paths are as a table, one row per host's
certificate, the port peers reach the box on, and one row per file it wrote:

```
where things live
  data    /srv/s5n/data    exists  254:0  xfs  1.8 TiB free
  config  /srv/s5n/config  new     254:0  xfs  1.8 TiB free
· box.example.com  Let's Encrypt · resolves to 203.0.113.7
· torrent port   picked    30013
✓ grant          20 keys
· values         already says this
✓ directories    12 made
```

`box apply` names the release it pulled, shows the diff as the review, and ends
with one row per thing that happened. A run with nothing to do is one note.

```
apply
✓ chart          s5n 0.6.3
s5n in s5n-alice · 1 change
~ wiring.timeoutSeconds: 300 -> 600
apply s5n in s5n-alice? [y/N] y
✓ storage        4 objects · 1s
✓ s5n 0.6.3      from 0.6.2 · 11 apps · 41s
✓ wiring         38 checks
```

A refusal is one `✗` row with its consequences under it, two columns in:

```
✗ /etc/k0s/k0s.yaml exists
  $ sudo mv /etc/k0s/k0s.yaml /etc/k0s/k0s.yaml.before-box
  then run this again
```

## The migration's screens

`box migrate` has more to say than any other command, so it says the counts and
writes the page. The report is one sectioned plain-text file with a summary at the
top: the snapshot carries it as `report.txt`, which travels with the one file an
operator moves between hosts, and the same bytes sit beside the snapshot as
`migrate-<time>-<mode>-report.txt`, which is what a person opens with `less`. One
builder writes both, so they cannot disagree. `box migrate apply` writes its own
page beside the same snapshot (`…-apply-report.txt`), because the rows it knows --
the keys it keeps, the connections it adopts by id, the notes -- are not facts
discovery could have read.

A read, and where its page is:

```
box migrate discover · survey from 2026-11-23 04:46 UTC
✓ read           12 apps · 14 rewritten · 3 left as it is
✓ decided        17
· left           31 workloads and mounts behind
· snapshot       /var/lib/s5n/migrate/migrate-20261123T044620Z-survey.tar.gz
· report         /var/lib/s5n/migrate/migrate-20261123T044620Z-survey-report.txt
                 $ less /var/lib/s5n/migrate/migrate-20261123T044620Z-survey-report.txt
! defaults       2 defaults taken · the report names each one
```

A cutover that finds the source still running: one line of state and the one
command that stops it, written for the cluster box found, and nothing after it.

```
! 12 apps running · stop them, then run this again
  $ sudo k0s kubectl -n default scale deployment --replicas=0 \
      autobrr maintainerr overseerr peerbanhelper plex qbittorrent \
      qui radarr radarr4k sonarr sonarr4k tautulli
```

The two other screens this command converted are the one confirm and the report
itself. The confirm is:

```
box migrate apply · s5n-alice · final snapshot from 2026-11-23 04:50 UTC

apps           carries    version             changes
  autobrr      9.0 GiB    1.91.1              login → s5n
  plex         48.0 GiB   1.43.5              its whole data: artwork too
  qbittorrent  570.0 MiB  5.1 → 5.3  ↑        1,565 paths
  qui          16.0 MiB   1.36.1              login → s5n
  radarr       1.0 GiB    6.0.4               keeps download client "qBT",
                                              keeps its API key
  sonarr       1.0 GiB    4.0.14 → 5.0  ↑     keeps its API key
  ✓ data     Downloads → downloads · library → media
  ✓ library  4K/Movies → movies-4k · 4K/TV → tv-4k · Movies → movies · TV → tv
  ! radarr, sonarr  "Deluge" (172.17.0.1:10365) and "rT" (172.17.0.1:443) stay
                    as it is: "Deluge" is a Deluge client and this box runs only
                    qBittorrent, so a rewrite would point it at an app that does
                    not speak its protocol
1 note · 2 other workloads stay behind · 2 values left as they are · --details

install, then move these 6 apps in? [y/N]
```

The apps past the eighth fold into one `+ 5 more` row that names them. The notes
are a count, not a list: the page has them once each. `--details` prints the
per-rewrite list for someone checking one row.

The version column is what the source ran and what this release runs, read from
the source's image tag and the chart's pinned image. `↑` is an upgrade, a value
with no arrow is the same version, and a version box could not read is `?` and
judges nothing. The fourth part of a tag and beyond is a build number (Plex's
1.43.5.11079), so it is in neither the column nor the comparison: two builds of
one version are one version.

A release older than the version that wrote the source's config is a downgrade:
an older app is not one to trust with a newer database, so the run stops before
anything moves, names each app with both versions, and gives the one command that
runs it anyway.

```
✗ sonarr  4.0.29 → 4.0.14
          the config is newer than the app
  $ box migrate apply /var/lib/s5n/migrate/migrate-20261123T045038Z-final.tar.gz --yolo
· nothing has moved
```

`--yolo` is that word: the confirm then carries one `!` row per downgraded app
instead of the refusal, and the run goes ahead.

```
! sonarr  4.0.29 → 4.0.14 · the config is newer than the app
```

The report's shape, which is the same for a read and for the run that applies it:

```
s5n migration report · final · migrate-20261123T045038Z-final.tar.gz
read 2026-11-23 04:50 UTC by box 0.6.2

summary
  apps       6
  addresses  11 rewritten · 3 left as it is
  data       2 renames · 4 library directories
  decisions  17
  notes      2
  left       31 workloads and mounts

apps
  autobrr      from autobrr in legacy · config /var/data/autobrr
  ...

addresses
  qbittorrent.legacy:8080  → qbittorrent · a Service of the source cluster
  10.244.0.94:32400        → plex · Maintainerr's Plex connection
  172.17.0.1:10365         left as it is · a Deluge client, and this box runs
                           qBittorrent
```

An answer an earlier read gave is not asked again; it is one row:

```
connections
✓ maintainerr  10.244.0.94:32400 → plex
```

And the replace path's teardown is commands, not prose: the k0s recipe removes the
`/etc/k0s/k0s.yaml` that `k0s reset` leaves behind, because the next bootstrap
refuses over it.

```
! this cluster is yours to replace: box removes no cluster it did not install
  $ sudo k0s stop; sudo k0s reset; sudo rm -f /etc/k0s/k0s.yaml
  $ sudo systemctl disable --now k0scontroller; sudo rm -f /etc/systemd/system/k0scontroller.service /usr/local/bin/k0s
  $ box migrate apply /var/lib/s5n/migrate/migrate-20261123T045038Z-final.tar.gz
```

## Rows that repeat, and the end of a run

A question opens with the question mark in the gutter, on both paths: the
terminal form and the plain prompt.

```
? install the platform layer and alice's seedbox?  [y/N]  y
```

Rows that differ only by their name are one fact and its size (rules 3 and 4).
A box's four host certificates share one decision, so they are one row:

```
· certificates   Let's Encrypt for 4 hosts · tls.issuer=letsencrypt
```

A block's name column is as wide as the longest name in it, in both renderers,
so every row's facts start in one column. One long name widens the block; it
never moves a row's facts to a line of their own.

What a run leaves a person is one block at its end. The label carries the one
mark, each row says what is left, and the command that clears it hangs under
the row it belongs to.

```
! to finish
  certificates  add a DNS record for box.example.test, plex.box.example.test
                $ box init
  updates       run this from cron every 15 minutes
                $ box update --timer
```

The live path report is the same table the transcript prints, with the two
facts that carry meaning coloured: whether the path is there, and the line that
says writing to it can sink the host.

```
where things live
  data    /srv/s5n/data    new     254:0  xfs  1.8 TiB free
  config  /srv/s5n/config  exists  254:0  xfs  1.8 TiB free
```

A value a migration leaves as it is says `kept` in the state column and gives
one reason. The sentence explaining what a rewrite would have done is in the
report's own section, where there is room for it (rule 7).
