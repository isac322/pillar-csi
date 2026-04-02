// Package names provides deterministic Kubernetes resource name generation
// derived from E2E test case IDs. Every TC ID maps to a unique, DNS-label-safe
// namespace name and object name prefix so that no two test cases ever share
// mutable Kubernetes state.
//
// Naming invariants
//
//   - Namespace:    e2e-tc-{normalized_id}-{hash8}   ≤ 63 chars
//   - ObjectPrefix: tc-{normalized_id}-{hash8}        ≤ 63 chars
//   - ResourceName: {ObjectPrefix}-{suffix}            ≤ 63 chars
//
// Normalization: lowercase the TC ID and replace every character that is not
// [a-z0-9] with a hyphen, then collapse consecutive hyphens into one, and
// strip leading/trailing hyphens.
//
// Uniqueness: the trailing 8-character hex digest is the first 8 characters
// of the SHA-256 hash of the raw (un-normalized) TC ID string, so even if two
// different raw IDs normalize to the same slug they will still differ.
package names

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// maxLen is the Kubernetes DNS-label length limit.
const maxLen = 63

var (
	nonAlphaNum   = regexp.MustCompile(`[^a-z0-9]+`)
	leadTrailDash = regexp.MustCompile(`^-+|-+$`)
)

// hash8 returns the first 8 hex characters of the SHA-256 of s.
func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:4]) // 4 bytes → 8 hex chars
}

// normalize converts a TC ID into a DNS-label-safe slug.
//
//  1. Lower-case the string.
//  2. Replace every run of non-alphanumeric characters with a single hyphen.
//  3. Strip any leading or trailing hyphens.
func normalize(tcID string) string {
	s := strings.ToLower(tcID)
	s = nonAlphaNum.ReplaceAllString(s, "-")
	s = leadTrailDash.ReplaceAllString(s, "")
	return s
}

// fitSlug truncates slug so that prefix + "-" + slug + "-" + hash8 fits in
// maxLen characters. prefix does NOT include a trailing separator.
func fitSlug(prefix, slug, suffix string) string {
	// layout: prefix + "-" + slug + "-" + suffix
	fixed := len(prefix) + 1 + 1 + len(suffix) // prefix + "-" + "-" + hash8
	available := maxLen - fixed
	if available <= 0 {
		// Edge case: prefix alone is already near maxLen – drop slug entirely.
		slug = ""
	} else if len(slug) > available {
		slug = slug[:available]
		// Strip a trailing hyphen that may appear after truncation.
		slug = strings.TrimRight(slug, "-")
	}
	return slug
}

// Namespace returns the unique Kubernetes namespace name for tcID.
//
// Format:  e2e-tc-{slug}-{hash8}
// The slug portion is truncated so the whole string is ≤ 63 characters.
func Namespace(tcID string) string {
	const prefix = "e2e-tc"
	h := hash8(tcID)
	slug := fitSlug(prefix, normalize(tcID), h)
	if slug == "" {
		return fmt.Sprintf("%s-%s", prefix, h)
	}
	return fmt.Sprintf("%s-%s-%s", prefix, slug, h)
}

// ObjectPrefix returns a short prefix suitable for any Kubernetes object name
// that belongs to tcID.
//
// Format:  tc-{slug}-{hash8}
func ObjectPrefix(tcID string) string {
	const prefix = "tc"
	h := hash8(tcID)
	slug := fitSlug(prefix, normalize(tcID), h)
	if slug == "" {
		return fmt.Sprintf("%s-%s", prefix, h)
	}
	return fmt.Sprintf("%s-%s-%s", prefix, slug, h)
}

// ResourceName returns a complete Kubernetes object name for tcID with the
// given suffix. The result is ObjectPrefix(tcID) + "-" + suffix, truncated
// to 63 characters. A trailing hyphen produced by truncation is removed.
func ResourceName(tcID, suffix string) string {
	base := ObjectPrefix(tcID) + "-" + suffix
	if len(base) <= maxLen {
		return base
	}
	truncated := strings.TrimRight(base[:maxLen], "-")
	return truncated
}
