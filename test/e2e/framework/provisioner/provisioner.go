// Package provisioner defines the BackendProvisioner interface — the
// extensibility contract for plugging new storage backend types into the E2E
// test pipeline's backend provisioning phase.
//
// # Design
//
// The E2E pipeline performs backend provisioning as a dedicated phase that runs
// after Kind cluster creation and before test execution. New backend types
// (ZFS, LVM, iSCSI, NVMe-oF, and future backends) plug into this phase by
// implementing [BackendProvisioner] and passing the implementation to
// [Pipeline.AddBackend]. No framework code changes are needed.
//
// The provisioning lifecycle is:
//
//  1. Pipeline.RunAll calls Provision on each registered BackendProvisioner.
//  2. Provision returns a [registry.Resource] capturing teardown logic.
//  3. The Resource is registered with a [registry.Registry] for cleanup.
//  4. Tests run against the provisioned backends.
//  5. Registry.Cleanup destroys each Resource in reverse registration order.
//
// # Soft-skip semantics
//
// When a required kernel module or container tool is absent, Provision should
// return (nil, nil) rather than an error. The Pipeline records this as a
// skipped backend and continues provisioning remaining backends. Test specs
// that depend on the absent backend are responsible for checking the returned
// [ProvisionResult.Resource] for nil and skipping accordingly.
//
// # Adding a new backend
//
// To add a hypothetical NVMe-oF backend without modifying any framework code:
//
//	// 1. Implement BackendProvisioner.
//	type NVMeProvisioner struct {
//	    NodeContainer string
//	    NQN           string
//	}
//
//	func (n *NVMeProvisioner) BackendType() string { return "nvmeof" }
//
//	func (n *NVMeProvisioner) Provision(ctx context.Context) (registry.Resource, error) {
//	    ns, err := nvme.CreateNamespace(ctx, nvme.Options{…})
//	    if err != nil { return nil, err }
//	    return ns, nil
//	}
//
//	// 2. Register with the pipeline — no framework changes needed.
//	p := provisioner.NewPipeline()
//	p.AddBackend(&NVMeProvisioner{NodeContainer: "…", NQN: "…"})
//	results, err := p.RunAll(ctx, os.Stderr)
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/registry"
)

// BackendProvisioner is the extensibility contract for E2E backend setup.
//
// Any type that implements BackendProvisioner can be registered with a
// [Pipeline] and participate in the backend provisioning phase without any
// changes to the framework itself. This is the primary extensibility point for
// new storage backend types (ZFS, LVM, iSCSI, NVMe-oF, …).
//
// # Implementing BackendProvisioner
//
// A backend type satisfies BackendProvisioner by providing two methods:
//
//  1. BackendType — returns a human-readable string identifying the backend
//     (e.g. "zfs", "lvm", "iscsi", "nvmeof"). Used in log output and error
//     messages. Must be non-empty.
//
//  2. Provision — creates the ephemeral storage resources needed by the test
//     suite inside the Kind container node. On success it returns a
//     [registry.Resource] that captures teardown logic. On soft-skip (absent
//     kernel module or container tool) it returns (nil, nil). On hard error it
//     returns (nil, err).
//
// # Soft-skip vs. hard error
//
//   - (resource, nil): provisioning succeeded; tests may use this backend.
//   - (nil, nil):      soft skip; required module/tool absent; tests using this
//     backend should skip rather than fail.
//   - (nil, err):      hard error; provisioning failed unexpectedly; the
//     Pipeline reports this as a failure.
//
// # Idempotency and atomicity
//
// Provision MUST perform best-effort cleanup of any partially-created resources
// before returning an error, so that a failed provisioning attempt leaves the
// container in a clean state.
type BackendProvisioner interface {
	// BackendType returns a human-readable identifier for this backend type.
	// Examples: "zfs", "lvm", "iscsi", "nvmeof".
	// Must not return an empty string.
	BackendType() string

	// Provision creates the ephemeral storage resources for the test run.
	//
	// Returns:
	//   - (resource, nil)  on success — resource implements [registry.Resource].
	//   - (nil, nil)       on soft skip — kernel module or container tool absent.
	//   - (nil, err)       on hard error — unexpected provisioning failure.
	Provision(ctx context.Context) (registry.Resource, error)
}

// ProvisionResult records the outcome of provisioning a single backend.
type ProvisionResult struct {
	// BackendType is the type identifier returned by BackendProvisioner.BackendType.
	BackendType string

	// Resource is the provisioned resource, or nil when the backend was skipped.
	Resource registry.Resource

	// Skipped is true when Provision returned (nil, nil) — soft skip.
	Skipped bool

	// Err is non-nil when Provision returned a hard error.
	Err error

	// Duration is the wall-clock time taken by Provision.
	Duration time.Duration
}

// Pipeline orchestrates provisioning across multiple backend types.
//
// Backends are registered via [Pipeline.AddBackend]. [Pipeline.RunAll]
// provisions all registered backends concurrently (or sequentially, if
// ordering constraints require it), collects results, and returns an error
// when any backend fails with a hard error.
//
// Pipeline is NOT safe for concurrent use during the registration phase;
// call AddBackend before RunAll.
//
// The zero value is NOT usable. Create a Pipeline via [NewPipeline].
type Pipeline struct {
	provisioners []BackendProvisioner
}

// NewPipeline creates an empty Pipeline ready to receive backends via
// [Pipeline.AddBackend].
func NewPipeline() *Pipeline {
	return &Pipeline{}
}

// AddBackend registers a BackendProvisioner with the pipeline.
//
// Passing a nil provisioner is a safe no-op — nil provisioners are silently
// ignored. The order of AddBackend calls determines the provisioning order
// (backends registered first are provisioned first).
func (p *Pipeline) AddBackend(prov BackendProvisioner) {
	if prov == nil {
		return
	}
	p.provisioners = append(p.provisioners, prov)
}

// BackendCount returns the number of registered backends.
func (p *Pipeline) BackendCount() int {
	if p == nil {
		return 0
	}
	return len(p.provisioners)
}

// RunAll provisions all registered backends sequentially and returns the
// combined results.
//
// RunAll logs progress to output (pass io.Discard or nil for silent operation).
// The returned slice has one entry per registered backend, in registration
// order.
//
// RunAll returns a non-nil error only when one or more backends returned a
// hard error from Provision. Soft-skip results (nil, nil from Provision) are
// NOT treated as errors — their ProvisionResult.Skipped field is set to true.
//
// On a hard error, RunAll continues provisioning the remaining backends rather
// than aborting early. All errors are collected and returned together via
// errors.Join so that the caller sees a complete picture of which backends
// failed.
//
// Calling RunAll on a nil *Pipeline is a safe no-op that returns (nil, nil).
func (p *Pipeline) RunAll(ctx context.Context, output io.Writer) ([]ProvisionResult, error) {
	if p == nil {
		return nil, nil
	}
	if output == nil {
		output = io.Discard
	}

	results := make([]ProvisionResult, 0, len(p.provisioners))
	var errs []error

	for _, prov := range p.provisioners {
		btype := prov.BackendType()

		start := time.Now()
		_, _ = fmt.Fprintf(output, "[provisioner] provisioning backend %q …\n", btype)

		res, err := prov.Provision(ctx)
		dur := time.Since(start)

		result := ProvisionResult{
			BackendType: btype,
			Resource:    res,
			Duration:    dur,
		}

		switch {
		case err != nil:
			result.Err = err
			errs = append(errs, fmt.Errorf("backend %q: %w", btype, err))
			_, _ = fmt.Fprintf(output, "[provisioner] backend %q FAILED after %s: %v\n",
				btype, dur.Round(time.Millisecond), err)

		case res == nil:
			result.Skipped = true
			_, _ = fmt.Fprintf(output, "[provisioner] backend %q skipped (kernel module or tool absent)\n", btype)

		default:
			_, _ = fmt.Fprintf(output, "[provisioner] backend %q provisioned in %s: %s\n",
				btype, dur.Round(time.Millisecond), res.Description())
		}

		results = append(results, result)
	}

	return results, errors.Join(errs...)
}

// RunAllConcurrent provisions all registered backends concurrently using
// goroutines and returns results in registration order.
//
// Unlike RunAll (which is sequential), RunAllConcurrent launches all Provision
// calls simultaneously. This is safe when backends are independent — for
// example, ZFS pool creation and LVM VG creation do not share any resources
// and can proceed in parallel, saving the sum-minus-max of their durations.
//
// Results are always returned in registration order (same slice layout as
// RunAll) even though provisioning is concurrent.  The result at index i
// corresponds to the provisioner registered at position i.
//
// Error handling follows the same rules as RunAll: soft-skip (nil, nil) sets
// Skipped=true; hard errors are collected across all goroutines and joined.
// A context cancellation cancels all in-flight Provision calls.
//
// Calling RunAllConcurrent on a nil *Pipeline is a safe no-op.
func (p *Pipeline) RunAllConcurrent(ctx context.Context, output io.Writer) ([]ProvisionResult, error) {
	if p == nil {
		return nil, nil
	}
	if output == nil {
		output = io.Discard
	}

	n := len(p.provisioners)
	if n == 0 {
		return nil, nil
	}

	// Pre-allocate a fixed-size results slice so each goroutine can write its
	// own slot without synchronisation on the slice itself.
	results := make([]ProvisionResult, n)
	errs := make([]error, n)

	// Wrap output in a mutex so concurrent goroutines can log without
	// interleaving lines.
	var mu sync.Mutex
	safeWrite := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintf(output, format, args...)
	}

	var wg sync.WaitGroup
	for i, prov := range p.provisioners {
		wg.Add(1)
		go func(idx int, prov BackendProvisioner) {
			defer wg.Done()

			btype := prov.BackendType()
			start := time.Now()
			safeWrite("[provisioner] provisioning backend %q …\n", btype)

			res, err := prov.Provision(ctx)
			dur := time.Since(start)

			result := ProvisionResult{
				BackendType: btype,
				Resource:    res,
				Duration:    dur,
			}

			switch {
			case err != nil:
				result.Err = err
				errs[idx] = fmt.Errorf("backend %q: %w", btype, err)
				safeWrite("[provisioner] backend %q FAILED after %s: %v\n",
					btype, dur.Round(time.Millisecond), err)

			case res == nil:
				result.Skipped = true
				safeWrite("[provisioner] backend %q skipped (kernel module or tool absent)\n", btype)

			default:
				safeWrite("[provisioner] backend %q provisioned in %s: %s\n",
					btype, dur.Round(time.Millisecond), res.Description())
			}

			results[idx] = result
		}(i, prov)
	}
	wg.Wait()

	return results, errors.Join(errs...)
}

// RegisterResources registers all non-nil provisioned Resources from results
// with reg. Skipped and failed results are silently ignored.
//
// This is a convenience helper for the common pattern of registering all
// provisioned resources with the suite Registry after RunAll returns.
func RegisterResources(reg *registry.Registry, results []ProvisionResult) {
	if reg == nil {
		return
	}
	for _, r := range results {
		if r.Resource != nil && !r.Skipped && r.Err == nil {
			reg.Register(r.Resource)
		}
	}
}
