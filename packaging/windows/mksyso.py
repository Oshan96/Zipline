"""Build a Windows resource .syso (COFF object with the application icon and version info) without
external tools.

usage: python3 packaging/windows/mksyso.py assets/icon.ico cmd/zipline [version]
Writes rsrc_windows_amd64.syso and rsrc_windows_arm64.syso, which `go build` links automatically. The
version info (what Explorer's Properties → Details shows, and what code signing services ask for) takes
the version given, or appVersion from cmd/zipline/version.go; packaging/package.sh gives each build its
full version.
"""
import re, struct, sys, os

RT_ICON, RT_GROUP_ICON, RT_VERSION, LANG = 3, 14, 16, 0x0409


def read_ico(path):
    b = open(path, "rb").read()
    _, typ, n = struct.unpack_from("<HHH", b, 0)
    assert typ == 1
    imgs = []
    for i in range(n):
        w, h, colors, _, planes, bpp, size, off = struct.unpack_from("<BBBBHHII", b, 6 + 16 * i)
        imgs.append(dict(w=w, h=h, colors=colors, planes=planes, bpp=bpp, data=b[off:off + size]))
    return imgs


def pad4(b):
    return b + b"\0" * (-len(b) % 4)


def node(key, value=b"", wtype=0, children=(), value_len=None):
    """One VS_VERSIONINFO block: its length, the value's length, its type (1: text), its key, then the value
    and the blocks inside it, each on a 4-byte boundary."""
    b = pad4(struct.pack("<HHH", 0, 0, wtype) + (key + "\0").encode("utf-16-le")) + value
    for c in children:
        b = pad4(b) + c
    return struct.pack("<HHH", len(b), len(value) if value_len is None else value_len, wtype) + b[6:]


def text(key, s):
    return node(key, (s + "\0").encode("utf-16-le"), 1, value_len=len(s) + 1)


def version_info(version):
    parts = [int(x) for x in version.split(".")][:4]
    parts += [0] * (4 - len(parts))
    ms, ls = parts[0] << 16 | parts[1], parts[2] << 16 | parts[3]
    fixed = struct.pack("<13I", 0xFEEF04BD, 0x00010000, ms, ls, ms, ls, 0x3F, 0, 0x00040004, 1, 0, 0, 0)  # (an app, for Windows NT)
    strings = [("CompanyName", "Zipline"), ("FileDescription", "Zipline Download Manager"), ("FileVersion", version),
               ("InternalName", "Zipline"), ("LegalCopyright", "Copyright (c) 2026 Oshan. MIT License."),
               ("OriginalFilename", "Zipline.exe"), ("ProductName", "Zipline"), ("ProductVersion", version)]
    table = node("040904b0", wtype=1, children=[text(k, v) for k, v in strings])  # (US English, Unicode)
    return node("VS_VERSION_INFO", fixed, 0, [node("StringFileInfo", wtype=1, children=[table]),
                                                node("VarFileInfo", wtype=1, children=[node("Translation", struct.pack("<HH", LANG, 1200))])])


def build(imgs, machine, reltype, version):
    # resources: {type: [(id, data)]}
    group = struct.pack("<HHH", 0, 1, len(imgs))
    for i, im in enumerate(imgs, 1):
        group += struct.pack("<BBBBHHIH", im["w"], im["h"], im["colors"], 0, im["planes"] or 1, im["bpp"] or 32, len(im["data"]), i)
    res = {RT_ICON: [(i, im["data"]) for i, im in enumerate(imgs, 1)], RT_GROUP_ICON: [(1, group)], RT_VERSION: [(1, version_info(version))]}
    types = sorted(res)

    dir_size = lambda n: 16 + 8 * n
    # layout: root | type dirs | lang dirs | data entries | blobs
    off = dir_size(len(types))
    type_off = {}
    for t in types:
        type_off[t] = off
        off += dir_size(len(res[t]))
    lang_off = {}
    for t in types:
        for rid, _ in res[t]:
            lang_off[(t, rid)] = off
            off += dir_size(1)
    entry_off = {}
    for t in types:
        for rid, _ in res[t]:
            entry_off[(t, rid)] = off
            off += 16
    blob_off = {}
    for t in types:
        for rid, data in res[t]:
            off = (off + 7) & ~7
            blob_off[(t, rid)] = off
            off += len(data)
    total = (off + 7) & ~7

    buf = bytearray(total)
    def dirhdr(at, n):
        struct.pack_into("<IIHHHH", buf, at, 0, 0, 0, 0, 0, n)
    dirhdr(0, len(types))
    for i, t in enumerate(types):
        struct.pack_into("<II", buf, 16 + 8 * i, t, 0x80000000 | type_off[t])
        dirhdr(type_off[t], len(res[t]))
        for j, (rid, _) in enumerate(res[t]):
            struct.pack_into("<II", buf, type_off[t] + 16 + 8 * j, rid, 0x80000000 | lang_off[(t, rid)])
    relocs = []
    for t in types:
        for rid, data in res[t]:
            lo = lang_off[(t, rid)]
            dirhdr(lo, 1)
            struct.pack_into("<II", buf, lo + 16, LANG, entry_off[(t, rid)])
            eo = entry_off[(t, rid)]
            struct.pack_into("<IIII", buf, eo, blob_off[(t, rid)], len(data), 0, 0)
            relocs.append(eo)  # OffsetToData needs the section RVA added
            buf[blob_off[(t, rid)]:blob_off[(t, rid)] + len(data)] = data

    raw_ptr = 20 + 40
    reloc_ptr = raw_ptr + len(buf)
    sym_ptr = reloc_ptr + 10 * len(relocs)
    out = bytearray()
    out += struct.pack("<HHIIIHH", machine, 1, 0, sym_ptr, 1, 0, 0)
    out += struct.pack("<8sIIIIIIHHI", b".rsrc", 0, 0, len(buf), raw_ptr, reloc_ptr, 0, len(relocs), 0, 0x40000040)
    out += buf
    for r in relocs:
        out += struct.pack("<IIH", r, 0, reltype)
    out += struct.pack("<8sIhHBB", b".rsrc", 0, 1, 0, 3, 0)
    out += struct.pack("<I", 4)
    return bytes(out)


if __name__ == "__main__":
    imgs = read_ico(sys.argv[1])
    od = sys.argv[2]
    if len(sys.argv) > 3:
        version = sys.argv[3]
    else:
        src = open(os.path.join(os.path.dirname(__file__), "..", "..", "cmd", "zipline", "version.go")).read()
        version = re.search(r'appVersion = "([0-9.]+)"', src).group(1)
    assert re.fullmatch(r"[0-9]+(\.[0-9]+){0,3}", version), "version must look like 1.0 or 1.0.3"
    open(os.path.join(od, "rsrc_windows_amd64.syso"), "wb").write(build(imgs, 0x8664, 3, version))   # IMAGE_REL_AMD64_ADDR32NB
    open(os.path.join(od, "rsrc_windows_arm64.syso"), "wb").write(build(imgs, 0xAA64, 2, version))   # IMAGE_REL_ARM64_ADDR32NB
    print("ok", len(imgs), "icon sizes, version", version)
