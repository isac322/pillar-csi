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

package lio

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// CHAP authentication on LIO (verified against Linux v6.8):
//
//   - tpgt_N/attrib/authentication is the TPG switch (iscsit_ta_authentication
//     in drivers/target/iscsi/iscsi_target_tpg.c accepts 0 or 1; 1 removes
//     "None" from the TPG's offered AuthMethod).  With generate_node_acls=0
//     every session runs on an explicit node ACL, and
//     iscsi_conn_auth_required (iscsi_target_nego.c) takes the ACL's
//     attrib/authentication unless it is NA_AUTHENTICATION_INHERITED (-1), in
//     which case it takes the TPG attribute.  Every ACL is therefore pinned
//     to -1 so the TPG attribute is the single switch.
//   - The credentials CHAP checks for a normal session are the node ACL's
//     (iscsi_get_node_auth returns &nacl->node_auth for a non-dynamic ACL):
//     acls/<iqn>/auth/{userid,password} for the initiator's response and
//     {userid_mutual,password_mutual} for the target's response in mutual
//     CHAP (iscsi_target_configfs.c, lio_target_nacl_auth_attrs).
//   - The auth store (__DEF_NACL_AUTH_STR) rejects count >= 256
//     (MAX_USER_LEN/MAX_PASS_LEN in include/target/iscsi/iscsi_target_core.h),
//     copies the bytes verbatim up to the first NUL, and treats a value
//     starting with "NULL" as unset (clearing the NAF_* flag).  The show
//     prints the stored value followed by "\n".  A zero-length write never
//     reaches the store (configfs_write_iter in fs/configfs/file.c), so the
//     only way to clear a value is writing "NULL", which then reads back as
//     "NULL".  authenticate_target (read-only) is 1 only while both mutual
//     values are set.

const (
	// MaxCHAPValueLength is the longest CHAP user name or secret LIO
	// stores: MAX_USER_LEN/MAX_PASS_LEN (256) including the NUL.
	MaxCHAPValueLength = 255

	// The value chapUnset is what LIO's auth store treats as "not set".
	chapUnset = "NULL"

	// The value aclAuthInherited makes a node ACL inherit the TPG's
	// authentication attribute (NA_AUTHENTICATION_INHERITED).
	aclAuthInherited = "-1"
)

// CHAP holds the CHAP credentials configured on every node ACL of a target.
// Username/Password authenticate the initiator (one-way CHAP);
// MutualUsername/MutualPassword, when set, authenticate the target to the
// initiator (mutual CHAP).  The values are secrets: they never appear in
// errors or logs.
type CHAP struct {
	Username       string
	Password       string
	MutualUsername string
	MutualPassword string
}

// Mutual reports whether c configures mutual CHAP.
func (c *CHAP) Mutual() bool {
	return c.MutualUsername != "" || c.MutualPassword != ""
}

// Validate checks that LIO can store c exactly: user name and secret are
// required, the mutual pair is set together or not at all, and every value
// is 1..MaxCHAPValueLength bytes without NUL or newline and does not start
// with "NULL" (which LIO would treat as unset).  Errors name the field, never
// its value.
func (c *CHAP) Validate() error {
	fields := []struct{ name, value string }{
		{"username", c.Username},
		{"password", c.Password},
	}
	if c.Mutual() {
		fields = append(fields,
			struct{ name, value string }{"mutual username", c.MutualUsername},
			struct{ name, value string }{"mutual password", c.MutualPassword})
	}
	for _, f := range fields {
		err := validateCHAPValue(f.value)
		if err != nil {
			return fmt.Errorf("CHAP %s %w", f.name, err)
		}
	}
	return nil
}

func validateCHAPValue(v string) error {
	switch {
	case v == "":
		return errors.New("is required")
	case len(v) > MaxCHAPValueLength:
		return fmt.Errorf("is %d bytes, LIO accepts at most %d", len(v), MaxCHAPValueLength)
	case strings.ContainsAny(v, "\x00\n"):
		return errors.New("must not contain NUL or newline")
	case strings.HasPrefix(v, chapUnset):
		return fmt.Errorf("must not start with %q (LIO treats it as unset)", chapUnset)
	}
	return nil
}

func (t *Target) aclAuthDir(iqn string) string { return filepath.Join(t.aclDir(iqn), "auth") }

// ensureACLAuth pins the node ACL of iqn to the TPG's authentication switch
// and, when CHAP is set, writes its credentials: userid and password, and
// userid_mutual and password_mutual for mutual CHAP or cleared ("NULL") for
// one-way CHAP.  Every value is written only when it differs and is read
// back.  Without CHAP the ACL's credentials are left untouched; the TPG's
// authentication=0 does not require them.
func (t *Target) ensureACLAuth(iqn string) error {
	c := t.cfs()
	err := c.ensureAttr(filepath.Join(t.aclDir(iqn), "attrib", "authentication"), aclAuthInherited)
	if err != nil {
		return fmt.Errorf("node ACL %q: %w", iqn, err)
	}
	if t.CHAP == nil {
		return nil
	}
	mutualUser, mutualPassword := chapUnset, chapUnset
	if t.CHAP.Mutual() {
		mutualUser, mutualPassword = t.CHAP.MutualUsername, t.CHAP.MutualPassword
	}
	dir := t.aclAuthDir(iqn)
	for _, kv := range [][2]string{
		{"userid", t.CHAP.Username},
		{"password", t.CHAP.Password},
		{"userid_mutual", mutualUser},
		{"password_mutual", mutualPassword},
	} {
		err = c.ensureSecretAttr(filepath.Join(dir, kv[0]), kv[1])
		if err != nil {
			return fmt.Errorf("node ACL %q CHAP: %w", iqn, err)
		}
	}
	return nil
}

// readSecretAttr reads an auth attribute exactly: LIO's show appends one
// "\n" to the stored bytes, which is the only thing removed.
func (c configfs) readSecretAttr(path string) (string, error) {
	data, err := c.fs.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("configfs read %q: %w", path, err)
	}
	return strings.TrimSuffix(strings.TrimRight(string(data), "\x00"), "\n"), nil
}

// ensureSecretAttr writes value to the auth attribute at path unless it
// already holds it, and reads it back.  A value of chapUnset also accepts an
// empty attribute (a fresh ACL never had it set).  Errors never contain the
// value.
func (c configfs) ensureSecretAttr(path, value string) error {
	got, err := c.readSecretAttr(path)
	if err != nil {
		return err
	}
	if got == value || (value == chapUnset && got == "") {
		return nil
	}
	err = c.fs.WriteFile(path, value)
	if err != nil {
		return fmt.Errorf("configfs write %q: %w", path, err)
	}
	got, err = c.readSecretAttr(path)
	if err != nil {
		return fmt.Errorf("configfs verify %q: %w", path, err)
	}
	if got != value {
		return fmt.Errorf("configfs verify %q: read-back differs from the written value", path)
	}
	return nil
}
