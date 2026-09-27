/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package csi

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// decodeMkfsOptions decodes the paramMkfsOptions value — a JSON array of
// strings, one mkfs argv element each.  An empty raw value yields no options.
//
// The value is written by CreateVolume from the resolved filesystem
// mkfsOptions (PillarStorageClass spec.filesystem, or a StorageClass / PVC
// filesystem document), travels unchanged in the PV VolumeContext, and is
// decoded again by NodeStageVolume.
func decodeMkfsOptions(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var opts []string
	err := json.Unmarshal([]byte(raw), &opts)
	if err != nil {
		return nil, fmt.Errorf("decode %s %q as a JSON string array: %w", paramMkfsOptions, raw, err)
	}
	return opts, nil
}

// parseMkfsOptions decodes raw (decodeMkfsOptions) and validates the options
// for fsType (validateMkfsOptions).
func parseMkfsOptions(fsType, raw string) ([]string, error) {
	opts, err := decodeMkfsOptions(raw)
	if err != nil {
		return nil, err
	}
	err = validateMkfsOptions(fsType, opts)
	if err != nil {
		return nil, err
	}
	return opts, nil
}

// mkfsFlag describes one allowed mkfs option.
type mkfsFlag struct {
	// takesValue reports whether the flag consumes a value, either attached
	// ("-L<label>") or as the next element ("-L", "<label>").
	takesValue bool
	// subopts, when non-nil, lists the allowed keys of a comma-separated
	// key[=value] value (e.g. mke2fs -E, mkfs.xfs -d).
	subopts []string
	// deniedValues lists comma-separated value tokens that are rejected
	// (e.g. the journal_dev feature of mke2fs -O).
	deniedValues []string
}

// mkfsSubSize is the "size" sub-option shared by several mkfs flags.
const mkfsSubSize = "size"

// mkfsFlags is the allowlist of mkfs options per filesystem type.
//
// The mkfs binary runs as root on the node with the host /dev visible, and PVC
// annotations are writable by namespace users, so only options that tune
// the filesystem being created on the volume are accepted.  Everything that
// makes mkfs open, read or write another file or device is excluded: an
// external journal, log or realtime device (mke2fs -J device=, which also
// accepts LABEL= and UUID= references; mkfs.xfs -l logdev=, -r rtdev=,
// -d name=/file=), content to copy in (mke2fs -d, mkfs.xfs -p), bad-block
// lists and undo files (mke2fs -l, -z), config files (mkfs.xfs -c); so is
// everything that would not leave the expected filesystem on the volume
// (dry runs -n / -N, -V, mke2fs -S, -t, -O journal_dev, -E offset=).
// Positional arguments are rejected too: mkfs would take them as the device
// or its size.
var mkfsFlags = map[string]map[string]mkfsFlag{
	defaultFsType: {
		"-b": {takesValue: true},
		"-C": {takesValue: true},
		"-D": {},
		"-e": {takesValue: true},
		"-E": {takesValue: true, subopts: []string{
			"assume_storage_prezeroed", "discard", "encoding", "encoding_flags",
			"lazy_itable_init", "lazy_journal_init", "no_copy_xattrs", "nodiscard",
			"num_backup_sb", "orphan_file_size", "packed_meta_blocks", "quotatype",
			"resize", "root_owner", "stride", "stripe_width", "stripe-width", "test_fs",
		}},
		"-F": {},
		"-g": {takesValue: true},
		"-G": {takesValue: true},
		"-i": {takesValue: true},
		"-I": {takesValue: true},
		"-j": {},
		"-J": {takesValue: true, subopts: []string{"fast_commit_size", "location", mkfsSubSize}},
		"-L": {takesValue: true},
		"-m": {takesValue: true},
		"-M": {takesValue: true},
		"-N": {takesValue: true},
		"-o": {takesValue: true},
		"-O": {takesValue: true, deniedValues: []string{"journal_dev", "^journal_dev"}},
		"-q": {},
		"-r": {takesValue: true},
		"-T": {takesValue: true},
		"-U": {takesValue: true},
		"-v": {},
	},
	xfsFsType: {
		"-b": {takesValue: true, subopts: []string{mkfsSubSize}},
		"-d": {takesValue: true, subopts: []string{
			"agcount", "agsize", "concurrency", "cowextsize", "daxinherit",
			"extszinherit", "noalign", mkfsSubSize, "su", "sunit", "sw", "swidth",
		}},
		"-f": {},
		"-i": {takesValue: true, subopts: []string{
			"align", "attr", "exchange", "maxpct", "nrext64", "projid32bit", mkfsSubSize, "sparse",
		}},
		"-K": {},
		"-l": {takesValue: true, subopts: []string{
			"agnum", "internal", "lazy-count", mkfsSubSize, "su", "sunit", "version",
		}},
		"-L": {takesValue: true},
		"-m": {takesValue: true, subopts: []string{
			"autofsck", "bigtime", "crc", "finobt", "inobtcount", "metadir", "reflink", "rmapbt", "uuid",
		}},
		"-n": {takesValue: true, subopts: []string{"ftype", "parent", mkfsSubSize, "version"}},
		"-q": {},
		"-s": {takesValue: true, subopts: []string{mkfsSubSize}},
	},
}

// validateMkfsOptions checks opts against the mkfsFlags allowlist of fsType.
// Every element must be a single-letter flag ("-X"); a flag that takes a
// value carries it attached ("-Lname") or as the next element.  Clustered
// flags ("-Fq") are rejected: list each flag separately.  The arguments are
// later passed to mkfs as separate argv elements (no shell).
func validateMkfsOptions(fsType string, opts []string) error {
	flags, ok := mkfsFlags[fsType]
	if !ok && len(opts) > 0 {
		return fmt.Errorf("mkfs options are not supported for filesystem type %q", fsType)
	}
	for i := 0; i < len(opts); i++ {
		opt := opts[i]
		if len(opt) < 2 || opt[0] != '-' || opt[1] == '-' {
			return fmt.Errorf("mkfs option %d %q is not a single-letter flag such as \"-L\": "+
				"positional arguments and long options are not accepted", i, opt)
		}
		name := opt[:2]
		spec, allowed := flags[name]
		if !allowed {
			return fmt.Errorf("mkfs option %q is not allowed for %s (allowed: %s)",
				name, fsType, strings.Join(allowedFlagNames(flags), " "))
		}
		value := opt[2:]
		if !spec.takesValue {
			if value != "" {
				return fmt.Errorf("mkfs option %d %q: %s takes no value; list each flag separately", i, opt, name)
			}
			continue
		}
		if value == "" {
			i++
			if i == len(opts) {
				return fmt.Errorf("mkfs option %s for %s is missing its value", name, fsType)
			}
			value = opts[i]
		}
		err := validateMkfsValue(fsType, name, spec, value)
		if err != nil {
			return err
		}
	}
	return nil
}

// validateMkfsValue checks the value of one allowed mkfs flag.
func validateMkfsValue(fsType, name string, spec mkfsFlag, value string) error {
	if strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
		return fmt.Errorf("mkfs option %s for %s has an empty or invalid value %q", name, fsType, value)
	}
	for token := range strings.SplitSeq(value, ",") {
		if slices.Contains(spec.deniedValues, token) {
			return fmt.Errorf("mkfs option %s %q is not allowed for %s", name, token, fsType)
		}
		if spec.subopts == nil {
			continue
		}
		key, _, _ := strings.Cut(token, "=")
		if !slices.Contains(spec.subopts, key) {
			return fmt.Errorf("mkfs option %s sub-option %q is not allowed for %s (allowed: %s)",
				name, key, fsType, strings.Join(spec.subopts, ","))
		}
	}
	return nil
}

// allowedFlagNames returns the sorted flag names of an allowlist.
func allowedFlagNames(flags map[string]mkfsFlag) []string {
	names := make([]string, 0, len(flags))
	for n := range flags {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
