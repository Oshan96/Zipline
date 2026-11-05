"""Check that Windows programs carry an Authenticode signature: their PE certificate table isn't empty.

usage: python3 packaging/windows/signed.py FILE...   (exits 1 if one isn't signed)
The release workflow runs it on what SignPath hands back, before anything is published.
"""
import struct, sys

unsigned = []
for path in sys.argv[1:]:
    b = open(path, "rb").read(4096)
    pe = struct.unpack_from("<I", b, 0x3C)[0]
    if b[:2] != b"MZ" or b[pe:pe + 4] != b"PE\0\0":
        sys.exit(f"{path}: not a Windows program")
    opt = pe + 24
    dirs = opt + (96 if struct.unpack_from("<H", b, opt)[0] == 0x10B else 112)  # (PE32 or PE32+)
    off, size = struct.unpack_from("<II", b, dirs + 8 * 4)  # the certificate table is the fifth data directory
    print(f"{path}: {'signed' if size else 'NOT signed'}")
    if not size:
        unsigned.append(path)
sys.exit(1 if unsigned else 0)
