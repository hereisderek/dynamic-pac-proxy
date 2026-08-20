#!/usr/bin/env python3
"""Builds a gzipped tar of a directory with every entry owned by root:root,
regardless of the uid/gid of whoever is running this build. Used instead of
plain `tar` because macOS's bsdtar has no simple --uid/--gid override for
create mode, and an .ipk's data/control tars must be root-owned.

Usage: _mktar.py <src-dir> <out.tar.gz>
"""
import sys
import tarfile


def reset_owner(tarinfo):
    tarinfo.uid = 0
    tarinfo.gid = 0
    tarinfo.uname = "root"
    tarinfo.gname = "root"
    return tarinfo


def main():
    if len(sys.argv) != 3:
        sys.exit(f"usage: {sys.argv[0]} <src-dir> <out.tar.gz>")
    src_dir, out_path = sys.argv[1], sys.argv[2]

    with tarfile.open(out_path, "w:gz") as tar:
        tar.add(src_dir, arcname=".", filter=reset_owner)


if __name__ == "__main__":
    main()
