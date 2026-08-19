package query

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The slot-lifecycle registry mutations for the blue-green rebuild. Placeholders
// are numbered in APPEARANCE order (SET columns first, the WHERE key last) so the
// statement binds correctly on BOTH numbered ($n) and positional (?) dialects —
// the same discipline as sqlBeginRebuild/sqlEndRebuild.

func sqlBeginSlotRebuild(d core.Dialect) string {
	return `UPDATE omnicore_mongo_views
   SET shadow_collection = ` + d.Placeholder(1) + `
WHERE view_name = ` + d.Placeholder(2)
}

func sqlFlipSlot(d core.Dialect) string {
	return `UPDATE omnicore_mongo_views
   SET active_collection = shadow_collection,
       shadow_collection = NULL
WHERE view_name = ` + d.Placeholder(1) + `
  AND shadow_collection IS NOT NULL`
}

func sqlAbortSlotRebuild(d core.Dialect) string {
	return `UPDATE omnicore_mongo_views
   SET shadow_collection = NULL
WHERE view_name = ` + d.Placeholder(1)
}

// beginSlotRebuild records the shadow slot a rebuild is building on the view's
// registry row. From this point Refresh reports shadow != "" for the view; the
// flip promotes it. Idempotent — re-recording the same shadow is a no-op write.
func beginSlotRebuild(ctx context.Context, q core.Querier, d core.Dialect, viewName, shadowName string) error {
	if err := core.Exec(q, ctx, sqlBeginSlotRebuild(d), shadowName, viewName); err != nil {
		return fmt.Errorf("begin slot rebuild %q: %w", viewName, err)
	}
	return nil
}

// flipSlot atomically promotes the recorded shadow slot to active and clears the
// shadow, in one registry write. The IS NOT NULL guard makes it a no-op when no
// rebuild is in flight, so a stray flip cannot blank the pointer. Callers Refresh
// the shared resolver afterwards so readers observe the new active slot.
func flipSlot(ctx context.Context, q core.Querier, d core.Dialect, viewName string) error {
	if err := core.Exec(q, ctx, sqlFlipSlot(d), viewName); err != nil {
		return fmt.Errorf("flip slot %q: %w", viewName, err)
	}
	return nil
}

// abortSlotRebuild clears the shadow pointer, ending dual-apply for the view
// cluster-wide (every pod stops writing the shadow after its next Refresh). The
// SyncEngine calls it when a shadow write fails past its bounded retry: the
// half-built shadow is abandoned rather than flipped, and the live path is
// untouched.
func abortSlotRebuild(ctx context.Context, q core.Querier, d core.Dialect, viewName string) error {
	if err := core.Exec(q, ctx, sqlAbortSlotRebuild(d), viewName); err != nil {
		return fmt.Errorf("abort slot rebuild %q: %w", viewName, err)
	}
	return nil
}
