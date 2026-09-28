//go:build linux

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
	"fmt"
	"slices"
	"strings"
)

// xfsProfileSections maps the sections of an mkfs.xfs configuration file to
// the command line flag that takes the same options (mkfs.xfs(8), OPTIONS).
var xfsProfileSections = map[string]string{
	"block":    "-b",
	"data":     "-d",
	"inode":    "-i",
	"log":      "-l",
	"metadata": "-m",
	"naming":   "-n",
	"realtime": "-r",
	"sector":   "-s",
}

// xfsProfileArgs converts the mkfs.xfs configuration file content profile
// into command line options, leaving out every option that formatOptions
// sets itself so the user value takes its place.
//
// The file is not passed with "-c options=": mkfs.xfs rejects an option set
// both in the file and on the command line ("respecified"), which would stop
// a user from overriding any profile value, e.g. opting in to "-i exchange=1".
func xfsProfileArgs(profile string, formatOptions []string) ([]string, error) {
	userSet := xfsUserSubopts(formatOptions)
	var args []string
	flag := ""
	var subopts []string
	flush := func() {
		if len(subopts) > 0 {
			args = append(args, flag, strings.Join(subopts, ","))
		}
		subopts = nil
	}
	for n, line := range strings.Split(profile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if section, ok := strings.CutPrefix(line, "["); ok {
			flush()
			section, ok = strings.CutSuffix(section, "]")
			flag = xfsProfileSections[strings.TrimSpace(section)]
			if !ok || flag == "" {
				return nil, fmt.Errorf("line %d: unsupported section %q", n+1, line)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" || flag == "" {
			return nil, fmt.Errorf("line %d: %q is not a key=value option of a section", n+1, line)
		}
		if !slices.Contains(userSet[flag], key) {
			subopts = append(subopts, key+"="+value)
		}
	}
	flush()
	return args, nil
}

// xfsUserSubopts returns the sub-option keys that the validated mkfs.xfs
// options opts set, per flag (e.g. "-i" → ["exchange"] for "-i exchange=1").
func xfsUserSubopts(opts []string) map[string][]string {
	flags := mkfsFlags[xfsFsType]
	set := make(map[string][]string)
	for i := 0; i < len(opts); i++ {
		if len(opts[i]) < 2 {
			continue
		}
		name, value := opts[i][:2], opts[i][2:]
		if !flags[name].takesValue {
			continue
		}
		if value == "" {
			i++
			if i == len(opts) {
				break
			}
			value = opts[i]
		}
		for token := range strings.SplitSeq(value, ",") {
			key, _, _ := strings.Cut(token, "=")
			set[name] = append(set[name], key)
		}
	}
	return set
}
