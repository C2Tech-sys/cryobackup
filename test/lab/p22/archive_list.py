"""archive_list.py - read a tar or zip archive from STANDARD INPUT (a pipe: the
writer cannot seek) and describe its entries, for the P22-S1 spike (item 6).

    restic dump -a tar <snap> <dir> | python archive_list.py tar
    restic dump -a zip <snap> <dir> | python archive_list.py zip

The tar is parsed in streaming mode ('r|'), as a consumer reading restic's
stdout would. Prints one line per entry (name, type, size, PAX keys) and a
summary; exit 0 when the archive parsed completely, 3 when it was truncated or
invalid. Only names and sizes are printed, never file contents.
"""
import io
import sys
import tarfile
import zipfile

TYPES = {tarfile.REGTYPE: "file", tarfile.AREGTYPE: "file", tarfile.DIRTYPE: "dir",
         tarfile.SYMTYPE: "symlink", tarfile.LNKTYPE: "hardlink", tarfile.CHRTYPE: "char",
         tarfile.BLKTYPE: "block", tarfile.FIFOTYPE: "fifo"}


def main() -> int:
    kind = sys.argv[1] if len(sys.argv) > 1 else "tar"
    raw = sys.stdin.buffer.read()
    print(f"bytes read from pipe: {len(raw)}")
    print(f"raw markers: ustar={raw.count(b'ustar')} PaxHeaders={raw.count(b'PaxHeaders')} "
          f"LongLink={raw.count(b'././@LongLink')} 'path='={raw.count(b' path=')}")
    if kind == "tar":
        n = 0
        try:
            with tarfile.open(fileobj=io.BytesIO(raw), mode="r|") as tf:
                for m in tf:
                    n += 1
                    pax = ",".join(sorted(m.pax_headers.keys())) if m.pax_headers else "-"
                    if n <= 40:
                        print(f"entry: {m.name!r} type={TYPES.get(m.type, m.type)} size={m.size} "
                              f"mode={oct(m.mode)} pax={pax} uname={m.uname!r}")
                    if m.isfile():
                        tf.extractfile(m).read()
        except (tarfile.TarError, EOFError, OSError) as exc:
            print(f"TRUNCATED/INVALID after {n} entries: {type(exc).__name__}: {exc}")
            return 3
        print(f"tar entries: {n}")
        return 0
    try:
        zf = zipfile.ZipFile(io.BytesIO(raw))
    except zipfile.BadZipFile as exc:
        print(f"INVALID zip: {exc}")
        return 3
    infos = zf.infolist()
    for i in infos[:40]:
        print(f"entry: {i.filename!r} size={i.file_size} compress={i.compress_type} "
              f"data_descriptor={bool(i.flag_bits & 0x08)} utf8={bool(i.flag_bits & 0x800)} dir={i.is_dir()}")
    bad = zf.testzip()
    print(f"zip entries: {len(infos)}; testzip first bad member: {bad}")
    return 0 if bad is None else 3


if __name__ == "__main__":
    sys.exit(main())
