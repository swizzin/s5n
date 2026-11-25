#!/usr/bin/env python3
"""Run one command on a terminal and answer its plain prompts.

box asks with --plain, so every question is a line whose last characters are a
prompt: a line prompt ends in ": " and its empty answer takes the default box was
given, and a yes/no question ends in "[y/N] " (or "[Y/n] "). This is how an e2e
drives a guided run without a person -- the install path a terminal takes, which
a pipe cannot reach because box refuses to ask where nobody can answer.

Usage:  answer-pty.py <command> [args...]

The answers are written once per prompt, and only after the prompt has been seen:
a pipe of ready-made lines would be buffered ahead by one prompt and lost to the
next one. Everything the command writes goes to stdout as it comes, so the
transcript is the one a person would have seen (with the echo of their typing).

The rules, in the order they are tried:

  * a question defaulting to yes gets an empty line, which is that default -- the
    fresh install this drives asks one, and a person there presses enter;
  * any other yes/no question gets "y";
  * anything else gets an empty line, which is the default box prefilled from the
    flags it was given.
"""

import errno
import os
import pty
import re
import select
import subprocess
import sys

PROMPTS = [
    # A question whose default is yes is answered with a bare enter: the e2e
    # proves the default a person's empty answer takes, not the keystroke.
    (re.compile(r"\[Y/n\] $"), b"\n"),
    (re.compile(r"\[[yY]/[nN]\] $"), b"y\n"),
    (re.compile(r": $"), b"\n"),
]


def main(argv):
    if len(argv) < 2:
        sys.stderr.write("usage: answer-pty.py <command> [args...]\n")
        return 2
    master, slave = pty.openpty()
    child = subprocess.Popen(argv[1:], stdin=slave, stdout=slave, stderr=slave, close_fds=True)
    os.close(slave)
    out = sys.stdout.buffer
    tail = b""
    try:
        while True:
            try:
                ready, _, _ = select.select([master], [], [], 0.2)
            except InterruptedError:
                continue
            if ready:
                try:
                    chunk = os.read(master, 4096)
                except OSError as err:
                    if err.errno == errno.EIO:
                        break
                    raise
                if not chunk:
                    break
                out.write(chunk)
                out.flush()
                # Only the last line can be a prompt, and its text is what says
                # which answer it wants.
                tail = (tail + chunk)[-512:]
            if child.poll() is not None and not ready:
                break
            text = tail.decode("utf-8", "replace").rsplit("\r\n", 1)[-1]
            for pattern, answer in PROMPTS:
                if pattern.search(text):
                    os.write(master, answer)
                    tail = b""
                    break
    finally:
        os.close(master)
    try:
        return child.wait(timeout=30)
    except subprocess.TimeoutExpired:
        child.kill()
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
