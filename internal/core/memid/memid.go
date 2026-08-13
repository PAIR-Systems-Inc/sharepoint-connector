// Package memid maps a source file id to a deterministic Goodmem memory id.
// Same (namespace, file id) ⇒ same memory id ⇒ idempotent inserts (no
// duplicate/orphan memories, and no need to search Goodmem by metadata).
//
// The namespace is per-source (e.g. "sharepoint.file.id", "google-drive.file.id") so
// two providers can't collide, and — more importantly — it is PERMANENT: it is
// the idempotency key, frozen the moment a source's first tenant goes live.
// Changing a source's namespace re-keys every memory in its space (a full
// delete + re-download + re-embed), so each provider must return a stable value
// from source.Source.MemNamespace().
package memid

import (
	"crypto/sha1"
	"fmt"
)

// namespaceDNS is the RFC 4122 Appendix C namespace UUID for DNS.
var namespaceDNS = [16]byte{
	0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// uuidV5 computes an RFC 4122 version-5 (SHA-1) UUID for name within namespace.
func uuidV5(ns [16]byte, name []byte) [16]byte {
	h := sha1.New()
	h.Write(ns[:])
	h.Write(name)
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // variant RFC 4122
	return u
}

func uuidString(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// FromFileID returns the deterministic Goodmem memoryId for fileID within the
// given per-source namespace. It mirrors the Python reference exactly:
//
//	uuid5(uuid5(NAMESPACE_DNS, namespace), file_id)
//
// For SharePoint the namespace is "sharepoint.file.id" (the Python-era value —
// preserved so existing memories keep their ids); Google Drive uses
// "google-drive.file.id". The namespace is PERMANENT (see the package doc).
func FromFileID(namespace, fileID string) string {
	ns := uuidV5(namespaceDNS, []byte(namespace))
	return uuidString(uuidV5(ns, []byte(fileID)))
}
