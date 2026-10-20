# libtorrent helper for Zipline's interoperability tests (run by torrent_test.go).
#   ltpeer.py make <dir> <tracker or -> <webseed or -> <piece size>  -> writes <dir>.torrent, prints {"torrent","magnet","hash"}
#   ltpeer.py seed <torrent file> <save path>                          -> seeds; prints {"port"} then progress lines
#   ltpeer.py leech <magnet or torrent> <save path> <peer host:port>   -> downloads from that peer; prints {"done": true} when complete
# Options (anywhere after the mode, all optional):
#   --enc=forced|disabled   encryption (MSE/PE) required (RC4 only) or turned off; default: libtorrent's own (enabled, either)
#   --dht=<host:port>       leech: also find peers through the DHT, bootstrapping from that node (the peer may then be "-")
#   --peers                 print {"peerlist": [...]} every second: connected peers with their encryption and where they came from
#   --unchoke=<n>           upload slots (0: never unchoke anyone; only the fast extension's allowed-fast pieces go out)
#   --allowed-fast=<n>      how many pieces each peer may ask for while choked (the fast extension's allowed-fast set)
#   --up=<bytes per second> upload limit
#   --utp                   the peer speaks uTP only (no TCP in or out)
#   --tcp                   the peer speaks TCP only (no uTP in or out)
#   --nopex                 no peer exchange (and no ut_metadata): the peer learns of others only from trackers and the like
#   --notracker             leech: ignore the .torrent file's trackers (so the peer it was told about is its only source)
import json, os, sys, time
import libtorrent as lt

opts = {}
args = [sys.argv[0]]
for a in sys.argv[1:]:
    if a.startswith("--"):
        k, _, v = a[2:].partition("=")
        opts[k] = v
    else:
        args.append(a)
sys.argv = args
UTP = "utp" in opts

def apply_opts(s):
    st = {}
    if opts.get("enc") == "forced":
        st.update(out_enc_policy=int(lt.enc_policy.pe_forced), in_enc_policy=int(lt.enc_policy.pe_forced), allowed_enc_level=int(lt.enc_level.pe_rc4))
    elif opts.get("enc") == "disabled":
        st.update(out_enc_policy=int(lt.enc_policy.pe_disabled), in_enc_policy=int(lt.enc_policy.pe_disabled))
    if "unchoke" in opts:
        st["unchoke_slots_limit"] = int(opts["unchoke"])
    if "allowed-fast" in opts:
        st["allowed_fast_set_size"] = int(opts["allowed-fast"])
    if "up" in opts:
        st["upload_rate_limit"] = int(opts["up"])
        st["ignore_limits_on_local_network"] = False  # (the tests run on 127.0.0.1, which counts as local)
    if st:
        s.apply_settings(st)

def peerlist(h):
    out = []
    for p in h.get_peer_info():
        src = [n for n, f in (("tracker", lt.peer_info.tracker), ("dht", lt.peer_info.dht), ("pex", lt.peer_info.pex)) if p.source & f]
        out.append({"ip": p.ip[0], "port": p.ip[1], "rc4": bool(p.flags & lt.peer_info.rc4_encrypted),
                    "obfuscated": bool(p.flags & lt.peer_info.plaintext_encrypted), "outgoing": bool(p.flags & lt.peer_info.local_connection),
                    "seed": bool(p.flags & lt.peer_info.seed), "source": src, "down": p.total_download})
    return out

def session(dht_boot=None):
    extra = {}
    if dht_boot is not None:
        extra = {"enable_dht": True, "dht_bootstrap_nodes": dht_boot, "dht_restrict_routing_ips": False, "dht_restrict_search_ips": False,
                 "dht_ignore_dark_internet": False, "dht_enforce_node_id": False, "dht_prefer_verified_node_ids": False}
    s = lt.session({
        "listen_interfaces": "127.0.0.1:0",
        "enable_dht": False, "enable_lsd": False, "enable_upnp": False, "enable_natpmp": False,
        "allow_multiple_connections_per_ip": True, "user_agent": "libtorrent-test",
        "announce_to_all_trackers": True, "announce_to_all_tiers": True,
        **({} if dht_boot is None else {}),
        "alert_mask": lt.alert.category_t.error_notification | lt.alert.category_t.status_notification | lt.alert.category_t.peer_notification | lt.alert.category_t.connect_notification,
    }, 2 if "nopex" in opts else 3)  # (3: the default plugins and features)
    if extra:
        s.apply_settings(extra)
    if UTP:
        s.apply_settings({"enable_outgoing_tcp": False, "enable_incoming_tcp": False, "enable_outgoing_utp": True, "enable_incoming_utp": True})
    if "tcp" in opts:
        s.apply_settings({"enable_outgoing_utp": False, "enable_incoming_utp": False})
    apply_opts(s)
    return s

def say(**kw):
    print(json.dumps(kw), flush=True)

mode = sys.argv[1]
if mode == "make":
    root, tracker, webseed, piece = sys.argv[2], sys.argv[3], sys.argv[4], int(sys.argv[5])
    fs = lt.file_storage()
    lt.add_files(fs, root)
    t = lt.create_torrent(fs, piece, flags=lt.create_torrent.v1_only)
    if tracker != "-":
        for tier, tr in enumerate(tracker.split(",")):
            t.add_tracker(tr, tier)
    if webseed != "-":
        t.add_url_seed(webseed)
    lt.set_piece_hashes(t, os.path.dirname(os.path.abspath(root)))
    data = lt.bencode(t.generate())
    path = os.path.abspath(root) + ".torrent"
    with open(path, "wb") as f:
        f.write(data)
    ti = lt.torrent_info(path)
    say(torrent=path, magnet=lt.make_magnet_uri(ti), hash=str(ti.info_hashes().v1))
elif mode == "dhtnode":
    s = session("")
    for _ in range(50):
        if s.is_dht_running():
            break
        time.sleep(0.1)
    say(port=s.listen_port())
    while True:
        time.sleep(1)
        say(nodes=s.status().dht_nodes)
elif mode == "seed":
    boot = sys.argv[4] if len(sys.argv) > 4 else None
    s = session(boot)
    p = lt.add_torrent_params()
    p.ti = lt.torrent_info(sys.argv[2])
    p.save_path = sys.argv[3]
    h = s.add_torrent(p)
    for _ in range(200):
        if h.status().is_seeding:
            break
        time.sleep(0.05)
    say(port=s.listen_port(), seeding=h.status().is_seeding)
    while True:
        st = h.status()
        say(peers=st.num_peers, uploaded=st.total_upload)
        if "peers" in opts:
            say(peerlist=peerlist(h))
        time.sleep(1)
elif mode == "leech":
    s = session(opts.get("dht"))
    src, save, peer = sys.argv[2], sys.argv[3], sys.argv[4]
    if src.startswith("magnet:"):
        p = lt.parse_magnet_uri(src)
    else:
        p = lt.add_torrent_params()
        p.ti = lt.torrent_info(src)
    p.save_path = save
    if "notracker" in opts:  # (added paused, so no tracker hears of it before they're dropped)
        p.flags = (p.flags | lt.torrent_flags.paused) & ~lt.torrent_flags.auto_managed
    h = s.add_torrent(p)
    if "notracker" in opts:
        h.replace_trackers([])
        h.resume()
    host, port = peer.rsplit(":", 1) if peer != "-" else ("", "0")
    say(port=s.listen_port())
    deadline = time.time() + 120
    last = 0
    while time.time() < deadline:
        if time.time() - last > 15 and peer != "-":
            last = time.time()
            try:
                h.connect_peer((host, int(port)))
            except Exception:
                pass
        st = h.status()
        if "peers" in opts:
            say(peerlist=peerlist(h))
        for a in s.pop_alerts():
            if os.environ.get("ZL_LTDEBUG") and ("peer" in a.what() or "error" in a.what()):
                say(alert=a.what(), msg=a.message())
        say(progress=st.progress, peers=st.num_peers, state=str(st.state), rate=st.download_rate)
        if st.is_seeding or (st.has_metadata and st.progress >= 1.0):
            say(done=True)
            break
        time.sleep(1)
    else:
        say(done=False)
    time.sleep(0.5)
