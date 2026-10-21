package main

import (
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"strings"
	"time"
)

// MEGA shared folders (mega.nz/folder/<id>#<key>, the older #F!<id>!<key>): MEGA's API lists the
// folder's nodes, each with its key encrypted with the folder's key (AES, block by block) and its
// name encrypted with its own key. A file in the folder is MEGA's own link for it,
// mega.nz/folder/<id>#<key>/file/<its handle>, downloaded like any MEGA file.

// megaFolderLink reads a folder link: the folder's handle and key, and the node it points to
// inside the folder ("" for the folder itself).
func megaFolderLink(u *url.URL) (fid, fkey, node string, ok bool) {
	p, frag := strings.TrimSuffix(u.Path, "/"), u.Fragment
	switch {
	case strings.HasPrefix(p, "/folder/"):
		fid = p[len("/folder/"):]
		var rest string
		fkey, rest, _ = strings.Cut(frag, "/")
		if _, n, found := strings.Cut(rest, "/"); found && (strings.HasPrefix(rest, "file/") || strings.HasPrefix(rest, "folder/")) {
			node = n
		}
	case strings.HasPrefix(frag, "F!"):
		parts := strings.Split(frag[2:], "!")
		fid = parts[0]
		if len(parts) > 1 {
			fkey = parts[1]
		}
		if len(parts) > 2 {
			node = parts[2]
		}
	default:
		return "", "", "", false
	}
	return fid, strings.TrimSpace(fkey), node, megaHandle.MatchString(fid)
}

// megaNode is a file or folder in a shared folder, decrypted.
type megaNode struct {
	h, parent, name string
	folder          bool
	size            int64
	key             []byte // 32 bytes for a file, 16 for a folder
}

// megaFolderNodes lists a shared folder's nodes; the first is the shared folder itself.
func megaFolderNodes(ctx context.Context, fid, fkey string) ([]megaNode, error) {
	if fkey == "" {
		return nil, permanentErr{errors.New("this MEGA link has no key (the part after #): copy the whole link")}
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.NewReplacer("+", "-", "/", "_", "=", "").Replace(fkey))
	if err != nil || len(key) != 16 {
		return nil, permanentErr{errors.New("the key in this MEGA folder link (the part after #) is damaged: copy the whole link again")}
	}
	block, _ := aes.NewCipher(key)
	out, err := megaCall(ctx, map[string]any{"a": "f", "c": 1, "ca": 1, "r": 1}, fid)
	if err != nil {
		return nil, err
	}
	var list struct {
		F []struct {
			H string `json:"h"`
			P string `json:"p"`
			T int    `json:"t"`
			A string `json:"a"`
			K string `json:"k"`
			S int64  `json:"s"`
		} `json:"f"`
	}
	json.Unmarshal(out, &list)
	var nodes []megaNode
	root := -1
	for _, f := range list.F {
		if f.T != 0 && f.T != 1 {
			continue // (the account's own special folders)
		}
		enc, isRoot := "", false // ("<owner>:<key>", several joined by "/"; the shared folder's own owner is itself)
		for _, pair := range strings.Split(f.K, "/") {
			if owner, k, ok := strings.Cut(pair, ":"); ok {
				enc, isRoot = k, isRoot || owner == f.H
			}
		}
		k, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(enc, "="))
		if err != nil || len(k)%16 != 0 || len(k) == 0 {
			continue
		}
		for i := 0; i < len(k); i += 16 { // (each block on its own)
			block.Decrypt(k[i:i+16], k[i:i+16])
		}
		n := megaNode{h: f.H, parent: f.P, folder: f.T == 1, size: f.S, key: k}
		nk := &megaKey{}
		copy(nk.aes[:], k)
		if len(k) == 32 {
			if fk, err := megaParseKey(base64.RawURLEncoding.EncodeToString(k)); err == nil {
				nk = fk
			}
		}
		n.name, _ = megaName(f.A, nk)
		if n.name == "" {
			continue // (a key that doesn't fit: not readable with this link)
		}
		if root < 0 && n.folder && isRoot {
			root = len(nodes)
		}
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return nil, permanentErr{errors.New("the key in this MEGA folder link (the part after #) doesn't fit the folder: copy the whole link again")}
	}
	if root < 0 { // else the folder whose parent isn't in the list
		in := map[string]bool{}
		for _, n := range nodes {
			in[n.h] = true
		}
		for i, n := range nodes {
			if n.folder && !in[n.parent] {
				root = i
				break
			}
		}
	}
	if root > 0 {
		nodes[0], nodes[root] = nodes[root], nodes[0]
	}
	return nodes, nil
}

// megaFolder lists a shared MEGA folder (or the folder in it a link points to).
func megaFolder(ctx context.Context, raw string) (*cloudFolder, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errNotFolder
	}
	fid, fkey, node, ok := megaFolderLink(u)
	if !ok {
		return nil, errNotFolder
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	nodes, err := megaFolderNodes(ctx, fid, fkey)
	if err != nil {
		return nil, err
	}
	top := nodes[0]
	kids := map[string][]megaNode{}
	for _, n := range nodes {
		kids[n.parent] = append(kids[n.parent], n)
		if n.h == node {
			top = n
		}
	}
	if !top.folder {
		return nil, errNotFolder
	}
	cf := &cloudFolder{Service: "MEGA", Name: top.name}
	share := "https://mega.nz/folder/" + fid + "#" + fkey
	var walk func(dir, h string, depth int)
	walk = func(dir, h string, depth int) {
		for _, n := range kids[h] {
			rel := path.Join(dir, n.name)
			switch {
			case len(cf.Files) >= maxFolderFiles:
				cf.More = true
				return
			case n.folder && depth >= maxFolderDepth:
				cf.More = true
			case n.folder:
				walk(rel, n.h, depth+1)
			default:
				cf.Files = append(cf.Files, cloudFile{Path: rel, Size: n.size, URL: share + "/file/" + n.h})
			}
		}
	}
	walk("", top.h, 0)
	return cf, nil
}

// resolveMegaInFolder gets the address of a file in a shared folder (or says it's a folder).
func resolveMegaInFolder(ctx context.Context, fid, fkey, node string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	nodes, err := megaFolderNodes(ctx, fid, fkey)
	if err != nil {
		return "", err
	}
	var file *megaNode
	isFolder := node == "" || nodes[0].h == node
	for i := range nodes {
		if nodes[i].h == node {
			if nodes[i].folder {
				isFolder = true
			} else if len(nodes[i].key) == 32 {
				file = &nodes[i]
			}
		}
	}
	switch {
	case isFolder:
		return "", permanentErr{errCloudFolder}
	case file == nil:
		return "", permanentErr{errors.New("MEGA: The file you are trying to download is no longer in this folder")}
	}
	k, _ := megaParseKey(base64.RawURLEncoding.EncodeToString(file.key))
	out, err := megaCall(ctx, map[string]any{"a": "g", "g": 1, "ssl": 2, "n": node}, fid)
	if err != nil {
		return "", err
	}
	return megaAddress(out, k, file.name)
}
