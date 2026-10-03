// Package configdocs decodes and validates the shared configuration
// documents of pillar-csi.  The same YAML document shapes are accepted in
// every place a storage, protocol or filesystem setting can be written:
//
//   - CRD fields (PillarStore.spec.backend, PillarProtocol.spec.protocol,
//     PillarStorageClass.spec.filesystem and .spec.overrides) — validated by
//     CEL/enum markers at the API server and by webhooks;
//   - PVC annotations pillar-csi.bhyoo.com/{backend,protocol,filesystem};
//   - the same keys as parameters of a hand-written StorageClass;
//   - the agent's backend placement config file (agent.backends).
//
// Decoding is strict: an unknown field is rejected with its full path, a
// structural field in a per-volume document is rejected as such, and every
// union document must set exactly one member.  The same numeric domains,
// enums and defaults apply at every layer.
//
// Directory backend documents declare logicalPool and the trusted hostRoot.
// Both fields are structural. Directory overrides have no tunables, and the
// agent only adopts existing quota-bounded sources; it never provisions them.
package configdocs

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"

	pillarv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// Document keys
// ─────────────────────────────────────────────────────────────────────────────.
//
// The document keys double as PVC annotation keys and hand-written
// StorageClass parameter keys — one key name per configuration axis.

const (
	// BackendDocKey names the backend override document.
	BackendDocKey = "pillar-csi.bhyoo.com/backend"

	// ProtocolDocKey names the protocol override document.
	ProtocolDocKey = "pillar-csi.bhyoo.com/protocol"

	// FilesystemDocKey names the filesystem document.
	FilesystemDocKey = "pillar-csi.bhyoo.com/filesystem"
)

// ─────────────────────────────────────────────────────────────────────────────
// Field schema
// ─────────────────────────────────────────────────────────────────────────────.
//
// Every accepted key of every document is enumerated here.  Structural keys
// are known to the schema but cannot appear in per-volume documents; the
// decoder reports them as structural instead of merely unknown so the user
// learns which fields exist and why they cannot be set.

// Backend union member names.
const (
	memberZFS       = "zfs"
	memberLVM       = "lvm"
	memberDirectory = "directory"
)

// Protocol union member names.
const (
	memberNVMeOFTCP = "nvmeofTcp"
	memberISCSI     = "iscsi"
	memberNFS       = "nfs"
)
const (
	fieldPort = "port"
	fieldACL  = "acl"
)

// fieldKind classifies how a document field may be used.
type fieldKind int

const (
	// FieldTunable may be set at every layer, including per volume.
	fieldTunable fieldKind = iota

	// FieldStructural defines topology, identity or security anchors; it may
	// only be set on the owning CRD (PillarStore / PillarProtocol) or agent
	// config, never in an override document.
	fieldStructural
)

// fieldSpec describes one accepted key of a union member document.
type fieldSpec struct {
	// kind classifies the field for per-volume documents.
	kind fieldKind

	// validate checks the field's value domain (type, range, enum).  It
	// receives the full document path and the decoded YAML value.
	validate func(path string, v any) error

	// required marks a field every full (non-override) document must set.
	required bool
}

// memberSpec describes one member of a union document.
type memberSpec struct {
	name   string
	fields map[string]fieldSpec
}

// backendMembers is the schema of a backend document (PillarStore.spec.backend,
// agent config backends[] entries, and per-volume backend override documents).
var backendMembers = map[string]memberSpec{
	memberZFS: {
		name: memberZFS,
		fields: map[string]fieldSpec{
			"volumeType":    {kind: fieldStructural, validate: enumValue("zvol", "dataset")},
			"pool":          {kind: fieldStructural, validate: nonEmptyString, required: true},
			"parentDataset": {kind: fieldStructural, validate: stringValue},
			"properties":    {kind: fieldTunable, validate: stringMap},
		},
	},
	memberLVM: {
		name: memberLVM,
		fields: map[string]fieldSpec{
			"volumeGroup":      {kind: fieldStructural, validate: nonEmptyString, required: true},
			"thinPool":         {kind: fieldStructural, validate: stringValue},
			"provisioningMode": {kind: fieldTunable, validate: enumValue("linear", "thin")},
		},
	},
	memberDirectory: {
		name: memberDirectory,
		fields: map[string]fieldSpec{
			"logicalPool": {kind: fieldStructural, validate: nonEmptyString, required: true},
			"hostRoot":    {kind: fieldStructural, validate: nonEmptyString, required: true},
		},
	},
}

// protocolMembers is the schema of a protocol document
// (PillarProtocol.spec.protocol and per-volume protocol override documents).
var protocolMembers = map[string]memberSpec{
	memberNVMeOFTCP: {
		name: memberNVMeOFTCP,
		fields: map[string]fieldSpec{
			fieldPort:             {kind: fieldStructural, validate: intRange(1, 65535)},
			fieldACL:              {kind: fieldStructural, validate: boolValue},
			"maxQueueSize":        {kind: fieldTunable, validate: intRange(16, 1024)},
			"inCapsuleDataSize":   {kind: fieldTunable, validate: intRange(1024, math.MaxInt32)},
			"maxDataTransferSize": {kind: fieldTunable, validate: maxDataTransferSize},
			"ctrlLossTmo":         {kind: fieldTunable, validate: intRange(0, math.MaxInt32)},
			"reconnectDelay":      {kind: fieldTunable, validate: intRange(0, math.MaxInt32)},
		},
	},
	memberNFS: {
		name: memberNFS,
		fields: map[string]fieldSpec{
			"version": {kind: fieldStructural, validate: enumValue("4.2")},
			fieldPort: {kind: fieldStructural, validate: intRange(2049, 2049)},
			fieldACL:  {kind: fieldStructural, validate: boolValue},
			"squash":  {kind: fieldStructural, validate: enumValue("root", "none", "all")},
		},
	},
	memberISCSI: {
		name: memberISCSI,
		fields: map[string]fieldSpec{
			fieldPort:            {kind: fieldStructural, validate: intRange(1, 65535)},
			fieldACL:             {kind: fieldStructural, validate: boolValue},
			"loginTimeout":       {kind: fieldTunable, validate: intRange(1, math.MaxInt32)},
			"replacementTimeout": {kind: fieldTunable, validate: intRange(0, math.MaxInt32)},
			"noopOutInterval":    {kind: fieldTunable, validate: intRange(0, math.MaxInt32)},
			"noopOutTimeout":     {kind: fieldTunable, validate: intRange(0, math.MaxInt32)},
			// Auth is a security anchor of the PillarProtocol: a binding or
			// volume must not change which credentials a target requires.
			"auth": {kind: fieldStructural, validate: mappingValue},
		},
	},
}

// filesystemFields is the schema of a filesystem document
// (PillarStorageClass.spec.filesystem and the per-volume filesystem
// document).  Every filesystem field is per-volume tunable.
var filesystemFields = map[string]fieldSpec{
	"fsType":       {kind: fieldTunable, validate: enumValue("ext4", "xfs", "nfs")},
	"mkfsOptions":  {kind: fieldTunable, validate: stringList},
	"mountOptions": {kind: fieldTunable, validate: stringList},
	"periodicTrim": {kind: fieldTunable, validate: boolValue},
}

// ─────────────────────────────────────────────────────────────────────────────
// Public decoders
// ─────────────────────────────────────────────────────────────────────────────.

// DecodeBackendOverride decodes a backend override document (the
// pillar-csi.bhyoo.com/backend PVC annotation or StorageClass parameter, or
// PillarStorageClass.spec.overrides.backend written via YAML tooling).
// Source names the input in error messages (e.g. the annotation key).
// Structural placement fields and unknown fields are rejected with their
// full path; exactly one member must be set.
func DecodeBackendOverride(source, raw string) (*pillarv1alpha1.BackendOverrides, error) {
	doc, err := decodeUnion(source, raw, backendMembers, true)
	if err != nil || doc == nil {
		return nil, err
	}
	member, body := doc.member, doc.body
	out := &pillarv1alpha1.BackendOverrides{}
	switch member {
	case memberZFS:
		cfg := &pillarv1alpha1.ZFSBackendOverrides{}
		err := unmarshalMember(source, member, body, cfg)
		if err != nil {
			return nil, err
		}
		out.ZFS = cfg
	case memberLVM:
		cfg := &pillarv1alpha1.LVMBackendOverrides{}
		err := unmarshalMember(source, member, body, cfg)
		if err != nil {
			return nil, err
		}
		out.LVM = cfg
	case memberDirectory:
		out.Directory = &pillarv1alpha1.DirectoryBackendOverrides{}
	}
	return out, nil
}

// DecodeProtocolOverride decodes a protocol override document (the
// pillar-csi.bhyoo.com/protocol PVC annotation or StorageClass parameter, or
// PillarStorageClass.spec.overrides.protocol written via YAML tooling).
func DecodeProtocolOverride(source, raw string) (*pillarv1alpha1.ProtocolOverrides, error) {
	doc, err := decodeUnion(source, raw, protocolMembers, true)
	if err != nil || doc == nil {
		return nil, err
	}
	out := &pillarv1alpha1.ProtocolOverrides{}
	switch doc.member {
	case memberNVMeOFTCP:
		cfg := &pillarv1alpha1.NVMeOFTCPOverrides{}
		err := unmarshalMember(source, doc.member, doc.body, cfg)
		if err != nil {
			return nil, err
		}
		out.NVMeOFTCP = cfg
	case memberISCSI:
		cfg := &pillarv1alpha1.ISCSIOverrides{}
		err := unmarshalMember(source, doc.member, doc.body, cfg)
		if err != nil {
			return nil, err
		}
		out.ISCSI = cfg
	case memberNFS:
		cfg := &pillarv1alpha1.NFSOverrides{}
		err := unmarshalMember(source, doc.member, doc.body, cfg)
		if err != nil {
			return nil, err
		}
		out.NFS = cfg
	}
	return out, nil
}

// DecodeFilesystemDoc decodes a filesystem document
// (PillarStorageClass.spec.filesystem written via YAML tooling, or the
// pillar-csi.bhyoo.com/filesystem PVC annotation / StorageClass parameter).
func DecodeFilesystemDoc(source, raw string) (*pillarv1alpha1.FilesystemConfig, error) {
	fields, err := decodeObject(source, raw)
	if err != nil || fields == nil {
		return nil, err
	}
	err = checkFields(source, "", filesystemFields, fields, false)
	if err != nil {
		return nil, err
	}
	out := &pillarv1alpha1.FilesystemConfig{}
	err = unmarshalMember(source, "", fields, out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeBackendSpec decodes a full backend configuration document: the same
// shape as PillarStore.spec.backend, used for the agent's placement config
// file entries (agent.backends[]).  Exactly one member must be set, and the
// CRD defaults are applied (zfs.volumeType defaults to zvol,
// lvm.provisioningMode defaults to linear).
func DecodeBackendSpec(source, raw string) (*pillarv1alpha1.BackendSpec, error) {
	doc, err := decodeUnion(source, raw, backendMembers, false)
	if err != nil || doc == nil {
		return nil, err
	}
	member, body := doc.member, doc.body
	out := &pillarv1alpha1.BackendSpec{}
	switch member {
	case memberZFS:
		cfg := &pillarv1alpha1.ZFSBackendConfig{}
		err := unmarshalMember(source, member, body, cfg)
		if err != nil {
			return nil, err
		}
		if cfg.VolumeType == "" {
			cfg.VolumeType = pillarv1alpha1.ZFSVolumeTypeZvol
		}
		out.ZFS = cfg
	case memberLVM:
		cfg := &pillarv1alpha1.LVMBackendConfig{}
		err := unmarshalMember(source, member, body, cfg)
		if err != nil {
			return nil, err
		}
		if cfg.ProvisioningMode == "" {
			cfg.ProvisioningMode = pillarv1alpha1.LVMProvisioningModeLinear
		}
		out.LVM = cfg
	case memberDirectory:
		cfg := &pillarv1alpha1.DirectoryBackendConfig{}
		err := unmarshalMember(source, member, body, cfg)
		if err != nil {
			return nil, err
		}
		out.Directory = cfg
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Union decoding
// ─────────────────────────────────────────────────────────────────────────────.

// unionDoc is the validated content of a union document: the selected member
// name and its object body.
type unionDoc struct {
	member string
	body   map[string]any
}

// decodeUnion decodes a YAML union document: a mapping with exactly one key,
// whose name must be a known member and whose value must be a mapping of
// that member's fields.  When overridesOnly is true, structural fields are
// rejected instead of decoded.
//
// An empty or whitespace-only document decodes to (nil, nil) — an absent
// document is not an override.
func decodeUnion(
	source, raw string,
	members map[string]memberSpec,
	overridesOnly bool,
) (*unionDoc, error) {
	fields, err := decodeObject(source, raw)
	if err != nil || fields == nil {
		return nil, err
	}

	names := sortedKeys(fields)
	for _, name := range names {
		if _, ok := members[name]; !ok {
			return nil, fmt.Errorf("%s: unknown field %q (supported: %s)",
				source, name, memberNames(members))
		}
	}
	if len(names) != 1 {
		return nil, fmt.Errorf("%s: exactly one of %s must be set (got %s)",
			source, memberNames(members), describeKeys(names))
	}
	member := names[0]
	spec := members[member]

	body, err := objectValue(source, member, fields[member])
	if err != nil {
		return nil, err
	}
	err = checkFields(source, member, spec.fields, body, overridesOnly)
	if err != nil {
		return nil, err
	}
	if !overridesOnly {
		for _, name := range fieldNamesSorted(spec.fields) {
			if _, set := body[name]; spec.fields[name].required && !set {
				return nil, fmt.Errorf("%s: %s.%s is required", source, member, name)
			}
		}
	}
	return &unionDoc{member: member, body: body}, nil
}

// decodeObject unmarshals raw YAML into a mapping.  Empty input returns nil.
func decodeObject(source, raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil //nolint:nilnil // an absent document is not an error and has no content
	}
	// yaml.v3 rejects duplicate mapping keys; sigs.k8s.io/yaml would keep
	// the last value and silently drop the others.
	var v any
	err := UnmarshalYAML([]byte(raw), &v)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid YAML: %w", source, err)
	}
	if v == nil {
		return nil, nil //nolint:nilnil // a null document is an absent document
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected a YAML mapping, got %s", source, yamlKind(v))
	}
	return obj, nil
}

// UnmarshalYAML decodes YAML like sigs.k8s.io/yaml does for the values
// pillar-csi accepts, but rejects duplicate mapping keys instead of keeping
// the last one.  Plain scalars that YAML 1.1/1.2 would read as timestamps
// (e.g. a dataset named 2026-01-01) stay strings, as they do in a
// Kubernetes object or Helm value.
func UnmarshalYAML(data []byte, out any) error {
	var node yamlv3.Node
	err := yamlv3.Unmarshal(data, &node)
	if err != nil {
		return err //nolint:wrapcheck // callers add the document source
	}
	if node.Kind == 0 {
		return nil
	}
	timestampsAsStrings(&node)
	return node.Decode(out) //nolint:wrapcheck // callers add the document source
}

// timestampsAsStrings retags plain timestamp scalars as strings.
func timestampsAsStrings(n *yamlv3.Node) {
	if n.Kind == yamlv3.ScalarNode && n.Style == 0 && n.ShortTag() == "!!timestamp" {
		n.Tag = "!!str"
	}
	for _, c := range n.Content {
		timestampsAsStrings(c)
	}
}

// checkFields validates every key of obj against the field schema: unknown
// keys are rejected, structural keys are rejected in override documents, and
// each value passes the field's domain check.
func checkFields(
	source, prefix string,
	schema map[string]fieldSpec,
	obj map[string]any,
	overridesOnly bool,
) error {
	for _, key := range sortedKeys(obj) {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		spec, ok := schema[key]
		if !ok {
			return fmt.Errorf("%s: unknown field %s (supported: %s)",
				source, path, fieldNames(schema))
		}
		if overridesOnly && spec.kind == fieldStructural {
			return fmt.Errorf("%s: %s is structural and cannot be set per volume",
				source, path)
		}
		if obj[key] == nil {
			// An explicit null is the same as an omitted field (inherit),
			// matching how the API server treats null in a CRD object.
			delete(obj, key)
			continue
		}
		if spec.validate != nil {
			err := spec.validate(path, obj[key])
			if err != nil {
				return fmt.Errorf("%s: %w", source, err)
			}
		}
	}
	return nil
}

// unmarshalMember decodes an already key-validated object into the typed
// struct.  Sigs.k8s.io/yaml converts via JSON, so the CRD json tags apply.
func unmarshalMember(source, member string, body map[string]any, out any) error {
	raw, err := yaml.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: encode %s: %w", source, member, err)
	}
	err = yaml.Unmarshal(raw, out)
	if err != nil {
		path := member
		if path == "" {
			path = "document"
		}
		return fmt.Errorf("%s: %s: %w", source, path, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Value validators
// ─────────────────────────────────────────────────────────────────────────────.

func nonEmptyString(path string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("%s: expected a string, got %s", path, yamlKind(v))
	}
	if s == "" {
		return fmt.Errorf("%s: must not be empty", path)
	}
	return nil
}

func stringValue(path string, v any) error {
	if _, ok := v.(string); !ok {
		return fmt.Errorf("%s: expected a string, got %s", path, yamlKind(v))
	}
	return nil
}

func enumValue(allowed ...string) func(path string, v any) error {
	return func(path string, v any) error {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: expected a string, got %s", path, yamlKind(v))
		}
		if slices.Contains(allowed, s) {
			return nil
		}
		return fmt.Errorf("%s: unsupported value %q (supported: %s)",
			path, s, strings.Join(allowed, ", "))
	}
}

func boolValue(path string, v any) error {
	if _, ok := v.(bool); !ok {
		return fmt.Errorf("%s: expected a boolean, got %s", path, yamlKind(v))
	}
	return nil
}

func intRange(minimum, maximum int64) func(path string, v any) error {
	return func(path string, v any) error {
		n, ok := int64Value(v)
		if !ok {
			return fmt.Errorf("%s: expected an integer, got %s", path, yamlKind(v))
		}
		if n < minimum || n > maximum {
			return fmt.Errorf("%s: %d is out of range [%d, %d]", path, n, minimum, maximum)
		}
		return nil
	}
}

// maxDataTransferSize accepts 0 (no limit) or a power of two from
// pillarv1alpha1.MinMaxDataTransferSize to MaxMaxDataTransferSize: the nvmet
// port encodes the limit as a power-of-two multiple of 4 KiB.
func maxDataTransferSize(path string, v any) error {
	n, ok := int64Value(v)
	if !ok {
		return fmt.Errorf("%s: expected an integer, got %s", path, yamlKind(v))
	}
	if !pillarv1alpha1.IsValidMaxDataTransferSize(n) {
		return fmt.Errorf("%s: %d must be 0 (no limit) or a power of two from %d to %d",
			path, n, pillarv1alpha1.MinMaxDataTransferSize, pillarv1alpha1.MaxMaxDataTransferSize)
	}
	return nil
}

func stringMap(path string, v any) error {
	obj, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: expected a mapping of string values, got %s", path, yamlKind(v))
	}
	for k, mv := range obj {
		if _, ok := mv.(string); !ok {
			return fmt.Errorf("%s.%s: expected a string value, got %s", path, k, yamlKind(mv))
		}
	}
	return nil
}

func mappingValue(path string, v any) error {
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("%s: expected a mapping, got %s", path, yamlKind(v))
	}
	return nil
}

func stringList(path string, v any) error {
	list, ok := v.([]any)
	if !ok {
		return fmt.Errorf("%s: expected a list of strings, got %s", path, yamlKind(v))
	}
	for i, item := range list {
		if _, ok := item.(string); !ok {
			return fmt.Errorf("%s[%d]: expected a string, got %s", path, i, yamlKind(item))
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────.

// objectValue coerces a union member's value into a mapping.
func objectValue(source, member string, v any) (map[string]any, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s must be a mapping of its fields, got %s",
			source, member, yamlKind(v))
	}
	return obj, nil
}

// int64Value converts a YAML-decoded number to int64.
func int64Value(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == math.Trunc(n) && n >= math.MinInt64 && n <= math.MaxInt64 {
			return int64(n), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// yamlKind describes the YAML kind of a decoded value for error messages.
func yamlKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "a mapping"
	case []any:
		return "a list"
	case string:
		return fmt.Sprintf("string %q", v)
	case bool:
		return fmt.Sprintf("boolean %v", v)
	case int, int64, float64:
		return fmt.Sprintf("number %v", v)
	default:
		return fmt.Sprintf("%T", v)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func memberNames(members map[string]memberSpec) string {
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, " or ")
}

func fieldNames(schema map[string]fieldSpec) string {
	return strings.Join(fieldNamesSorted(schema), ", ")
}

func fieldNamesSorted(schema map[string]fieldSpec) []string {
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func describeKeys(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}
