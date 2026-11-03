#!/usr/bin/env python3
"""Build a single-file torrent for a media file, and the descriptor the mock indexer serves.

The grab test needs the torrent and the file on disk to agree byte for byte, and it
needs the media to be something ffprobe will accept, so it generates a real (tiny)
video with ffmpeg and hashes it here. Nothing is downloaded from anywhere.

Usage:
  make-torrent.py --file payload.mkv --name Release.Name.mkv \
      --announce http://indexer/announce --out torrent.bin --meta meta.json
"""
import argparse
import hashlib
import json

PIECE_LENGTH = 64 * 1024


def bencode(value):
    if isinstance(value, int):
        return b"i%de" % value
    if isinstance(value, bytes):
        return b"%d:%s" % (len(value), value)
    if isinstance(value, str):
        return bencode(value.encode())
    if isinstance(value, list):
        return b"l" + b"".join(bencode(v) for v in value) + b"e"
    if isinstance(value, dict):
        out = b"d"
        for key in sorted(value):
            out += bencode(key) + bencode(value[key])
        return out + b"e"
    raise TypeError(f"cannot bencode {type(value)!r}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--file", required=True)
    ap.add_argument("--name", required=True, help="file name inside the torrent")
    ap.add_argument("--announce", required=True)
    ap.add_argument("--out", required=True, help="where to write the .torrent")
    ap.add_argument("--meta", required=True, help="where to write the descriptor JSON")
    args = ap.parse_args()

    data = open(args.file, "rb").read()
    pieces = b"".join(
        hashlib.sha1(data[i : i + PIECE_LENGTH]).digest()
        for i in range(0, len(data), PIECE_LENGTH)
    )
    info = {
        "length": len(data),
        "name": args.name,
        "piece length": PIECE_LENGTH,
        "pieces": pieces,
    }
    torrent = bencode({"announce": args.announce, "info": info})
    infohash = hashlib.sha1(bencode(info)).hexdigest()

    with open(args.out, "wb") as fh:
        fh.write(torrent)
    meta = {"name": args.name, "size": len(data), "infohash": infohash}
    with open(args.meta, "w", encoding="utf-8") as fh:
        json.dump(meta, fh)
    print(json.dumps(meta))


if __name__ == "__main__":
    main()
