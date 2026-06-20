// Package sanity wraps the upstream kubernetes-csi/csi-test sanity suite so
// it can be executed against an in-process pillar-csi driver.
//
// The entire suite is gated behind the `csi_sanity` build tag so that the
// kubernetes-csi/csi-test dependency does not affect the default test/build
// matrix.  Run with:
//
//	go test -tags=csi_sanity ./test/sanity/...
//
// or via the convenience target:
//
//	make test-csi-sanity
package sanity
