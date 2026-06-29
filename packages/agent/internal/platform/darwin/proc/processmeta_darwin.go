//go:build darwin

// Package proc implements macOS process metadata lookups using the
// libproc API (proc_pidpath, proc_pidinfo) via CGO.
package proc

/*
#include <libproc.h>
#include <sys/proc_info.h>
*/
import "C"

import (
	"fmt"
	"os/user"
	"path/filepath"
	"strconv"
	"unsafe"
)

const maxProcPath = 4096

// Meta contains metadata about a process, resolved via libproc.
// Callers in the platform package map this into platform.ProcessMeta.
type Meta struct {
	PID      int
	Path     string // full executable path
	Name     string // short name (or CFBundleDisplayName for versioned helpers)
	BundleID string // macOS CFBundleIdentifier, or empty
	User     string // OS username
}

// ProcessInfo resolves process metadata for a given PID using macOS libproc APIs.
func ProcessInfo(pid int) (Meta, error) {
	meta := Meta{PID: pid}

	// Resolve executable path via proc_pidpath
	var pathBuf [maxProcPath]C.char
	// nolint:gocritic // underef false positive: gocritic inspects cgo-generated
	// wrapper that re-indexes our &pathBuf[0] argument; our source is correct.
	ret := C.proc_pidpath(C.int(pid), unsafe.Pointer(&pathBuf[0]), C.uint32_t(maxProcPath))
	if ret > 0 {
		meta.Path = C.GoString(&pathBuf[0])
		meta.Name = filepath.Base(meta.Path)
		meta.BundleID = DetectBundleID(meta.Path)
		// Defense: some Electron apps (Claude Desktop, Cursor)
		// nest the executable inside a Frameworks/Helpers tree where
		// the basename ends up looking like a version string ("2.1.141"
		// because that's the Helper directory name). Prefer the
		// containing .app bundle's display name when the basename
		// looks like a version (matches \d+\.\d+ ...). The .app's
		// Info.plist CFBundleName / CFBundleDisplayName is the actual
		// product name a user expects to see in audit rows.
		if LooksLikeVersionString(meta.Name) {
			if appName := BundleDisplayNameFromPath(meta.Path); appName != "" {
				meta.Name = appName
			}
		}
	}

	// Resolve process owner and short name via proc_pidinfo
	var bsdInfo C.struct_proc_bsdinfo
	infoSize := C.int(unsafe.Sizeof(bsdInfo))
	// nolint:gocritic // dupSubExpr false positive: cgo's _cgoCheckPointer
	// wrapper compares the same pointer twice (base vs base+0); our source has
	// no `==`. Column 122 in the lint output points past end-of-line because
	// the rule fires on the cgo-expanded form, not our source.
	ret = C.proc_pidinfo(C.int(pid), C.PROC_PIDTBSDINFO, 0, unsafe.Pointer(&bsdInfo), infoSize)
	if ret > 0 {
		uid := uint32(bsdInfo.pbi_uid)
		if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
			meta.User = u.Username
		}
		if meta.Name == "" {
			meta.Name = C.GoString(&bsdInfo.pbi_comm[0])
		}
	}

	if meta.Path == "" {
		return meta, fmt.Errorf("proc_pidpath failed for pid %d", pid)
	}
	return meta, nil
}

// LooksLikeVersionString matches names that look like a version
// number (e.g. "2.1.141", "1.0", "3.4.5-beta"). Used by ProcessInfo
// to decide whether to fall back to the containing .app bundle's
// display name.
func LooksLikeVersionString(s string) bool {
	if s == "" {
		return false
	}
	hasDigit := false
	hasDotOrLetter := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '.':
			hasDotOrLetter = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '-' || r == '_':
			// Allow "1.0-beta" / "v2.3" but disqualify "Cursor" /
			// "Chrome" — pure letters disqualify, mixed letters+digits
			// with a leading digit qualify.
			hasDotOrLetter = true
		default:
			return false
		}
	}
	if !hasDigit || !hasDotOrLetter {
		return false
	}
	// Must START with a digit; pure letter-strings like "Cursor" are
	// real names. "v2.3" has letter first → not a version match.
	first := s[0]
	return first >= '0' && first <= '9'
}

// BundleDisplayNameFromPath walks up from execPath looking for the
// nearest .app bundle and reads its CFBundleName / CFBundleDisplayName
// from Info.plist. Used by ProcessInfo as the fallback name when the
// raw basename looks like a version string. Returns empty when
// no .app ancestor exists or its Info.plist can't be parsed.
func BundleDisplayNameFromPath(execPath string) string {
	dir := execPath
	for {
		dir = filepath.Dir(dir)
		if dir == "/" || dir == "." {
			return ""
		}
		if !hasAppSuffix(dir) {
			continue
		}
		plistPath := filepath.Join(dir, "Contents", "Info.plist")
		data, err := readFile(plistPath)
		if err != nil {
			// Walk further up — Electron apps nest sub-app bundles
			// (e.g. ".../Cursor.app/Contents/Frameworks/.../
			// Cursor Helper.app/Contents/MacOS/2.1.141"); the
			// outer .app holds the real product name.
			continue
		}
		// Prefer CFBundleDisplayName over CFBundleName when present
		// — that's the user-visible name (e.g. "Cursor" instead of
		// "cursor"). Same simple plist scrape detectBundleID uses.
		if name := ScrapePlistKey(string(data), "CFBundleDisplayName"); name != "" {
			return name
		}
		if name := ScrapePlistKey(string(data), "CFBundleName"); name != "" {
			return name
		}
		// This .app didn't help; walk up to the enclosing .app
		// (Electron Helpers).
	}
}

// DetectBundleID extracts CFBundleIdentifier from an .app bundle if the
// executable is inside one.
func DetectBundleID(execPath string) string {
	dir := execPath
	for {
		dir = filepath.Dir(dir)
		if dir == "/" || dir == "." {
			break
		}
		if hasAppSuffix(dir) {
			plistPath := filepath.Join(dir, "Contents", "Info.plist")
			data, err := readFile(plistPath)
			if err != nil {
				break
			}
			content := string(data)
			idx := indexString(content, "<key>CFBundleIdentifier</key>")
			if idx < 0 {
				break
			}
			rest := content[idx:]
			start := indexString(rest, "<string>")
			end := indexString(rest, "</string>")
			if start >= 0 && end > start+8 {
				return rest[start+8 : end]
			}
			break
		}
	}
	return ""
}

// ScrapePlistKey is the dumb-string-search alternative to a real
// plist parser used by DetectBundleID. Adequate for the very narrow
// set of keys we actually look for and avoids pulling in a plist
// dependency.
func ScrapePlistKey(content, key string) string {
	keyTag := "<key>" + key + "</key>"
	idx := indexString(content, keyTag)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(keyTag):]
	start := indexString(rest, "<string>")
	end := indexString(rest, "</string>")
	if start < 0 || end <= start+len("<string>") {
		return ""
	}
	return trimSpace(rest[start+len("<string>") : end])
}

// IntPtrIfNonZero returns &v when v != 0, nil otherwise. Used to map
// shared/audit.AuditEvent's int64 token counts (zero == "unknown") to
// the *int columns proxy.InspectionResult uses (nil == SQL NULL).
func IntPtrIfNonZero(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}
