// Package rules holds what a folder may send to The Archivist, matching the archive's own
// rules (spec 0006 §4.2, spec 0012 §6.2), so the program never sends what the archive refuses.
package rules

import (
	"path"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MaxFileBytes is the archive's limit for one file.
const MaxFileBytes = 10 * 1024 * 1024

// ManifestPageSize is how many entries one manifest request may carry.
const ManifestPageSize = 5000

// RootPrefix starts the name of every folder Archivist Sync makes in the archive (spec 0012 §6.1).
const RootPrefix = "archivist-sync-"

// convertible are the formats the archive converts to Markdown, keeping only their text.
var convertible = map[string]bool{
	"pdf": true, "docx": true, "odt": true, "xlsx": true, "xls": true, "ods": true,
	"csv": true, "numbers": true, "html": true, "htm": true, "xml": true,
}

// clutter is what systems leave in folders; it's never sent, nor listed as left out.
var clutter = map[string]bool{
	".ds_store": true, "thumbs.db": true, "desktop.ini": true, "icon\r": true,
}

// Extension is a file's lower-case extension without its dot, or "".
func Extension(p string) string {
	name := path.Base(p)
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return ""
	}
	return strings.ToLower(name[dot+1:])
}

// Takes reports whether the archive takes a file of this name: Markdown, text or a format it
// converts.
func Takes(p string) bool {
	ext := Extension(p)
	return ext == "md" || ext == "txt" || convertible[ext]
}

// Clutter reports whether a file is system clutter: never sent and never listed.
func Clutter(name string) bool {
	lower := strings.ToLower(name)
	return clutter[lower] || strings.HasPrefix(name, "~$") || strings.HasPrefix(name, "._")
}

// Hidden reports whether a name is hidden: it starts with a dot.
func Hidden(name string) bool { return strings.HasPrefix(name, ".") }

// SafePath reports whether a relative, forward-slash path is one the archive accepts: no
// absolute, empty, hidden or traversing segments and no control characters or backslashes.
func SafePath(p string) bool {
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == '\\' {
			return false
		}
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || strings.HasPrefix(s, ".") {
			return false
		}
	}
	return true
}

var rootName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidRoot reports whether a root name is one the archive accepts.
func ValidRoot(root string) bool { return rootName.MatchString(root) }

var notRootChars = regexp.MustCompile(`[^a-z0-9._-]+`)

// RootFor names the archive folder for a folder on this computer: archivist-sync- and its name,
// lower-cased, accents removed, other characters as dashes (spec 0012 §6.1).
func RootFor(folderName string) string {
	decomposed := norm.NFKD.String(strings.ToLower(folderName))
	var b strings.Builder
	for _, r := range decomposed {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	slug := strings.Trim(notRootChars.ReplaceAllString(b.String(), "-"), "-._")
	if slug == "" {
		slug = "folder"
	}
	root := RootPrefix + slug
	if len(root) > 64 {
		root = root[:64]
	}
	return strings.TrimRight(root, "-._")
}

// NeedsDeletionConfirmation is the deletion guard (spec 0005 §5.3): a sync that would remove
// more than 20 documents and more than a quarter of the folder stops and asks first.
func NeedsDeletionConfirmation(deleteCount, keptCount int) bool {
	return deleteCount > 20 && float64(deleteCount) > float64(deleteCount+keptCount)/4
}
