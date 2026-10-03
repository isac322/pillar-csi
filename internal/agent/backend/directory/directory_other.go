//go:build !linux

package directory

import (
	"context"
	"fmt"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

func unsupported() error {
	return fmt.Errorf("directory backend: filesystem adoption is unsupported on non-Linux hosts")
}
func capacity(context.Context, string) (totalBytes, availableBytes int64, err error) {
	return 0, 0, unsupported()
}

type dirPin struct{}

func (*Backend) inspect(
	ctx context.Context,
	_ string,
	_ int64,
	_ *agentv1.FilesystemAdoption,
	_ bool,
) (*backend.ImportInspection, *dirPin, error) {
	if ctx == nil {
		return nil, nil, unsupported()
	}
	select {
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("directory backend context: %w", ctx.Err())
	default:
		return nil, nil, unsupported()
	}
}
func makePinned(*dirPin, *agentv1.FilesystemAdoption, int64, *Backend) (backend.PinnedFilesystem, error) {
	return nil, unsupported()
}
