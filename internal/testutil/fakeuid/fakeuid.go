// Package fakeuid makes the controller-runtime fake client behave like the
// Kubernetes API server with respect to object UIDs.
//
// The API server assigns every object a UID on creation; the fake client does
// not.  The pillar-csi controller uses the PillarVolumeState UID as the
// lifecycle identity of a volume's fencing token and refuses to send a token
// without one, so every fake client that stores PillarVolumeState objects must
// assign UIDs.
package fakeuid

import (
	"context"

	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Interceptor returns fake-client interceptor functions that assign a fresh
// UID to every object created without one.
func Interceptor() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(uuid.NewUUID())
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

// Assign sets a fresh UID on every object that has none and returns them, for
// objects seeded through ClientBuilder.WithObjects, which bypasses Create:
//
//	builder.WithObjects(fakeuid.Assign(target, pvs)...)
func Assign(objs ...client.Object) []client.Object {
	for _, obj := range objs {
		if obj.GetUID() == "" {
			obj.SetUID(uuid.NewUUID())
		}
	}
	return objs
}
