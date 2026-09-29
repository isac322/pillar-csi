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

// T5: NodeStageVolume records whether it ran mkfs on the SP13
// pillar_csi.node.format_and_mount span, with the mkfs run as an SP8 child.

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/isac322/pillar-csi/internal/telemetry"
)

// installSpanRecorder makes the global TracerProvider record every span for
// the duration of the test.
func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		err := tp.Shutdown(context.Background()) // t.Context() is already canceled in Cleanup
		if err != nil {
			t.Errorf("shut down tracer provider: %v", err)
		}
	})
	return sr
}

// stageTraced runs NodeStageVolume for a MOUNT volume of fsType on the fake
// device dev under a root span standing in for the SP9 server span, and
// returns the ended spans of that trace.
func stageTraced(
	t *testing.T, sr *tracetest.SpanRecorder, dev *fakeDeviceExec, fsType string,
) []sdktrace.ReadOnlySpan {
	t.Helper()
	km, _ := newFormatTestMounter(t, dev)
	srv := NewNodeServerWithStateDir("test-node", &mockConnector{devicePath: fakeDevice}, km, t.TempDir())

	ctx, root := telemetry.Tracer().Start(t.Context(), "csi.v1.Node/NodeStageVolume")
	_, err := srv.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "agent-1/nvmeof-tcp/zfs-zvol/tank/pvc-t5",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  mountCap(fsType),
		VolumeContext: mountVolumeContext(
			"nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-t5", "192.0.2.1"),
	})
	root.End()
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	var spans []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.SpanContext().TraceID() == root.SpanContext().TraceID() {
			spans = append(spans, s)
		}
	}
	return spans
}

func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func spanAttr(s sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// requireFormatSpan returns the format_and_mount span after checking its
// pillar_csi.mkfs.performed and pillar_csi.fs.detected attributes.
func requireFormatSpan(
	t *testing.T, spans []sdktrace.ReadOnlySpan, wantPerformed bool, wantDetected string,
) sdktrace.ReadOnlySpan {
	t.Helper()
	span := findSpan(spans, telemetry.SpanNodeFormatAndMount)
	if span == nil {
		t.Fatalf("no %s span in the NodeStageVolume trace", telemetry.SpanNodeFormatAndMount)
	}
	performed, ok := spanAttr(span, telemetry.KeyMkfsPerformed)
	if !ok || performed.AsBool() != wantPerformed {
		t.Errorf("%s = %v (set %v), want %v", telemetry.KeyMkfsPerformed, performed.String(), ok, wantPerformed)
	}
	detected, ok := spanAttr(span, telemetry.KeyFSDetected)
	if !ok || detected.AsString() != wantDetected {
		t.Errorf("%s = %q (set %v), want %q", telemetry.KeyFSDetected, detected.String(), ok, wantDetected)
	}
	return span
}

// TestNodeStageVolume_BlankDeviceRecordsMkfs verifies that staging a blank
// device sets pillar_csi.mkfs.performed=true and records the mkfs run as an
// "exec mkfs.xfs" child of the format_and_mount span.
func TestNodeStageVolume_BlankDeviceRecordsMkfs(t *testing.T) {
	sr := installSpanRecorder(t)
	spans := stageTraced(t, sr, &fakeDeviceExec{}, "xfs")

	format := requireFormatSpan(t, spans, true, "")
	mkfs := findSpan(spans, "exec mkfs.xfs")
	if mkfs == nil {
		t.Fatal(`no "exec mkfs.xfs" span in the NodeStageVolume trace`)
	}
	if mkfs.Parent().SpanID() != format.SpanContext().SpanID() {
		t.Errorf("exec mkfs.xfs parent = %s, want the format_and_mount span %s",
			mkfs.Parent().SpanID(), format.SpanContext().SpanID())
	}
	if mkfs.SpanKind() != trace.SpanKindInternal {
		t.Errorf("exec mkfs.xfs kind = %v, want internal", mkfs.SpanKind())
	}
}

// TestNodeStageVolume_FormattedDeviceSkipsMkfs verifies that staging a device
// that already carries a filesystem sets pillar_csi.mkfs.performed=false,
// reports the detected filesystem, and creates no mkfs span.
func TestNodeStageVolume_FormattedDeviceSkipsMkfs(t *testing.T) {
	sr := installSpanRecorder(t)
	spans := stageTraced(t, sr, &fakeDeviceExec{fsType: "ext4"}, "ext4")

	requireFormatSpan(t, spans, false, "ext4")
	for _, s := range spans {
		if strings.HasPrefix(s.Name(), "exec mkfs.") {
			t.Errorf("unexpected %q span for an already formatted device", s.Name())
		}
	}
}
