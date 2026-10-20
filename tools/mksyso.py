"""Build a Windows resource .syso (COFF object with an application icon) without external tools.

usage: python3 mksyso.py icon.ico out_dir
Writes rsrc_windows_amd64.syso and rsrc_windows_arm64.syso, which `go build` links automatically.
"""
import struct, sys, os

RT_ICON, RT_GROUP_ICON, LANG = 3, 14, 0x0409


def read_ico(path):
    b = open(path, "rb").read()
    _, typ, n = struct.unpack_from("<HHH", b, 0)
    assert typ == 1
    imgs = []
    for i in range(n):
        w, h, colors, _, planes, bpp, size, off = struct.unpack_from("<BBBBHHII", b, 6 + 16 * i)
        imgs.append(dict(w=w, h=h, colors=colors, planes=planes, bpp=bpp, data=b[off:off + size]))
    return imgs


def build(imgs, machine, reltype):
    # resources: {type: [(id, data)]}
    group = struct.pack("<HHH", 0, 1, len(imgs))
    for i, im in enumerate(imgs, 1):
        group += struct.pack("<BBBBHHIH", im["w"], im["h"], im["colors"], 0, im["planes"] or 1, im["bpp"] or 32, len(im["data"]), i)
    res = {RT_ICON: [(i, im["data"]) for i, im in enumerate(imgs, 1)], RT_GROUP_ICON: [(1, group)]}
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
    open(os.path.join(od, "rsrc_windows_amd64.syso"), "wb").write(build(imgs, 0x8664, 3))   # IMAGE_REL_AMD64_ADDR32NB
    open(os.path.join(od, "rsrc_windows_arm64.syso"), "wb").write(build(imgs, 0xAA64, 2))   # IMAGE_REL_ARM64_ADDR32NB
    print("ok", len(imgs), "icon sizes")
