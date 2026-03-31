//go:build e2e
// +build e2e

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

package framework

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// PollInterval and WaitTimeout are the active values used by all Wait*
// helpers.  They default to conservative values suitable for any environment
// and can be overridden via E2E_POLL_INTERVAL_MS and E2E_WAIT_TIMEOUT_S
// environment variables for faster feedback on fast machines.
//
// These values live in test code only (behind the e2e build tag) and have
// zero effect on production binaries.
var (
	PollInterval = initPollInterval()
	WaitTimeout  = initWaitTimeout()
)

func initPollInterval() time.Duration {
	ms, err := strconv.Atoi(os.Getenv("E2E_POLL_INTERVAL_MS"))
	if err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 500 * time.Millisecond
}

func initWaitTimeout() time.Duration {
	s, err := strconv.Atoi(os.Getenv("E2E_WAIT_TIMEOUT_S"))
	if err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 90 * time.Second
}

// ─────────────────────────────────────────────────────────────────────────────
// WaitForCondition
// ─────────────────────────────────────────────────────────────────────────────

// WaitForCondition polls obj (re-fetching it from the API server on each
// attempt) until the named condition has the desired status, or until the
// context is cancelled / timeout is exceeded.
//
// Parameters:
//   - condType   – the Condition.Type string, e.g. "Ready", "AgentConnected"
//   - wantStatus – desired metav1.ConditionStatus ("True", "False", "Unknown")
//   - timeout    – maximum wait duration; pass 0 to use WaitTimeout
//
// obj is updated in-place with the latest server state on every poll cycle.
// On success the caller can inspect obj.Status to read the final state.
//
// Supported object types: PillarTarget, PillarPool, PillarProtocol,
// PillarBinding, PillarVolume.  For other types the condition list is treated
// as always-empty and the function will time out.
func WaitForCondition(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	condType string,
	wantStatus metav1.ConditionStatus,
	timeout time.Duration,
) error {
	if timeout == 0 {
		timeout = WaitTimeout
	}

	key := client.ObjectKeyFromObject(obj)
	var lastMsg string

	err := wait.PollUntilContextTimeout(ctx, PollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if fetchErr := c.Get(ctx, key, obj); fetchErr != nil {
				if errors.IsNotFound(fetchErr) {
					lastMsg = fmt.Sprintf("%T %q: object not found", obj, key)
					return false, nil
				}
				return false, fetchErr
			}

			for _, cond := range conditionsOf(obj) {
				if cond.Type != condType {
					continue
				}
				if cond.Status == wantStatus {
					return true, nil
				}
				lastMsg = fmt.Sprintf("condition %q: status=%s reason=%s message=%s",
					condType, cond.Status, cond.Reason, cond.Message)
				return false, nil
			}

			lastMsg = fmt.Sprintf("condition %q not yet present on %T %q", condType, obj, key)
			return false, nil
		},
	)
	if err != nil {
		msg := fmt.Sprintf("WaitForCondition: %T %q [%s=%s]", obj, key, condType, wantStatus)
		if lastMsg != "" {
			msg += " — last observed: " + lastMsg
		}
		return fmt.Errorf("%s: %w", msg, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// WaitForReady
// ─────────────────────────────────────────────────────────────────────────────

// WaitForReady is a convenience wrapper around WaitForCondition that waits
// for the standard "Ready" condition to become metav1.ConditionTrue.
//
// Example:
//
//	pt := framework.NewExternalPillarTarget("t1", "10.0.0.1", 9500)
//	Expect(framework.Apply(ctx, c, pt)).To(Succeed())
//	Expect(framework.WaitForReady(ctx, c, pt, 2*time.Minute)).To(Succeed())
func WaitForReady(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	timeout time.Duration,
) error {
	return WaitForCondition(ctx, c, obj, "Ready", metav1.ConditionTrue, timeout)
}

// ─────────────────────────────────────────────────────────────────────────────
// WaitForDeletion
// ─────────────────────────────────────────────────────────────────────────────

// WaitForDeletion polls until obj is fully removed from the API server (i.e.
// Get returns NotFound), or the context / timeout is exceeded.
//
// This function does not issue a Delete call — call Delete or EnsureGone to
// trigger the deletion first.
func WaitForDeletion(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	timeout time.Duration,
) error {
	if timeout == 0 {
		timeout = WaitTimeout
	}

	key := client.ObjectKeyFromObject(obj)

	err := wait.PollUntilContextTimeout(ctx, PollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if fetchErr := c.Get(ctx, key, obj); fetchErr != nil {
				if errors.IsNotFound(fetchErr) {
					return true, nil
				}
				return false, fetchErr
			}
			return false, nil
		},
	)
	if err != nil {
		return fmt.Errorf("WaitForDeletion %T %q: %w", obj, key, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// WaitForField — generic field predicate
// ─────────────────────────────────────────────────────────────────────────────

// WaitForField polls obj (re-fetching it from the API server on each attempt)
// until predicate(obj) returns true, or the context / timeout is exceeded.
//
// This is the most flexible wait primitive.  Use it when the status field you
// need is not a standard Condition (e.g. waiting for a resolved IP address or
// a non-zero replica count).
//
// obj is updated in-place on every poll cycle.  predicate receives the
// refreshed object and should return true when the desired state is reached.
//
// Example — wait until PillarTarget has a resolved agent address:
//
//	err := framework.WaitForField(ctx, c, target, func(t *v1alpha1.PillarTarget) bool {
//	    return t.Status.ResolvedAddress != ""
//	}, 2*time.Minute)
//
// Example — wait until PillarPool reports available capacity:
//
//	err := framework.WaitForField(ctx, c, pool, func(p *v1alpha1.PillarPool) bool {
//	    return p.Status.Capacity != nil && p.Status.Capacity.Available != nil
//	}, 3*time.Minute)
func WaitForField[T client.Object](
	ctx context.Context,
	c client.Client,
	obj T,
	predicate func(T) bool,
	timeout time.Duration,
) error {
	if timeout == 0 {
		timeout = WaitTimeout
	}

	key := client.ObjectKeyFromObject(obj)

	err := wait.PollUntilContextTimeout(ctx, PollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if fetchErr := c.Get(ctx, key, obj); fetchErr != nil {
				if errors.IsNotFound(fetchErr) {
					return false, nil
				}
				return false, fetchErr
			}
			return predicate(obj), nil
		},
	)
	if err != nil {
		return fmt.Errorf("WaitForField %T %q: %w", obj, key, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// conditionsOf extracts the []metav1.Condition slice from the known pillar-csi
// object types.  Returns nil for unrecognised types.
func conditionsOf(obj client.Object) []metav1.Condition {
	switch o := obj.(type) {
	case *v1alpha1.PillarTarget:
		return o.Status.Conditions
	case *v1alpha1.PillarPool:
		return o.Status.Conditions
	case *v1alpha1.PillarProtocol:
		return o.Status.Conditions
	case *v1alpha1.PillarBinding:
		return o.Status.Conditions
	case *v1alpha1.PillarVolume:
		return o.Status.Conditions
	default:
		return nil
	}
}
