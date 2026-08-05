package maintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
)

// ======================================================================================
// Object Store Reconciliation
//
// The DB is authoritative for what exists; the object store is reconciled toward it (see DESIGN
// §2.2.1, §8.2.1). Neither artifact nor workspace deletion touches the store, so this is where
// every freed object is eventually reclaimed - and the only place in the system an object is
// ever deleted.

// objectListPageSize the number of object keys read per listing call.
//
// S3 pages listings natively at this size, so asking for it costs one server round trip per
// page and keeps the working set the sweep holds bounded regardless of how large the bucket is.
const objectListPageSize = 1000

/*
listObjectPage read one page of object under a prefix.

	@param ctx context.Context - execution context
	@param prefix string - the object key prefix to confine the listing to
	@param startAfter *string - the last key of the previous page, nil for the first page
	@returns the page of objects
*/
func (m *managerImpl) listObjectPage(
	ctx context.Context, prefix string, startAfter *string,
) ([]goutils.S3ObjectStat, error) {
	s3Client, err := m.s3Client(ctx)
	if err != nil {
		return nil, err
	}

	pageSize := objectListPageSize
	objects, err := s3Client.ListObjects(
		ctx, m.storeConfig.Bucket, &prefix, startAfter, &pageSize,
	)
	if err != nil {
		return nil, goutils.NewObjectStoreError(
			fmt.Sprintf("failed to list s3://%s/%s", m.storeConfig.Bucket, prefix), err, true,
		)
	}

	return objects, nil
}

/*
orphanCutoff the instant an object or entry must predate to be considered orphaned rather than
in flight.

Derived once per sweep from the timestamp handed in, rather than per decision from the wall
clock. A full-bucket pass takes as long as it takes, and a cutoff recomputed as it went would
judge the last page against a stricter line than the first - so whether an object survived would
depend on where in the listing it happened to fall.

	@param timestamp time.Time - the current timestamp
	@returns the grace window's trailing edge
*/
func (m *managerImpl) orphanCutoff(timestamp time.Time) time.Time {
	return timestamp.Add(-m.maintenanceConfig.OrphanedObjectAgeOut())
}

/*
objectAgedOut report whether an object has sat unassociated long enough to be reclaimed.

The age comes from the listing that produced the object, so there is no round trip here and no
way for the question to fail - which is what lets a whole sweep cost one call per page.

	@param cutoff time.Time - the grace window's trailing edge, from orphanCutoff
	@param object goutils.S3ObjectStat - the object to age, as the listing reported it
	@returns whether the object is old enough to reclaim
*/
func objectAgedOut(cutoff time.Time, object goutils.S3ObjectStat) bool {
	return object.LastModified.Before(cutoff)
}

/*
deleteObjects reclaim a batch of object keys.

Per-key failures are reported rather than returned as one error: a bulk delete is partially
successful by nature, and the keys that did go are gone whatever happened to the rest. The
object store treats deleting an absent key as a success, so a key another actor removed first
does not surface here at all.

	@param ctx context.Context - execution context
	@param objectKeys []string - the objects to reclaim
	@returns the number reclaimed
*/
func (m *managerImpl) deleteObjects(
	ctx context.Context, objectKeys []string,
) (int, []error) {
	if len(objectKeys) == 0 {
		return 0, nil
	}

	logTags := m.GetLogTagsForContext(ctx)

	s3Client, err := m.s3Client(ctx)
	if err != nil {
		return 0, []error{err}
	}

	perKey, err := s3Client.DeleteObjects(ctx, m.storeConfig.Bucket, objectKeys)

	var failures []error
	for objectKey, keyErr := range perKey {
		log.
			WithError(keyErr).
			WithFields(logTags).
			WithField("object_key", objectKey).
			Error("Failed to reclaim unassociated object")
		failures = append(failures, goutils.NewObjectStoreError(
			fmt.Sprintf("failed to delete s3://%s/%s", m.storeConfig.Bucket, objectKey),
			keyErr,
			true,
		))
	}

	if err != nil {
		// The per-key map is not exhaustive when the bulk delete itself failed, so nothing more
		// can be said about the keys it does not mention. Report none of the batch as reclaimed.
		return 0, append(failures, goutils.NewObjectStoreError(
			fmt.Sprintf("failed to bulk delete from s3://%s", m.storeConfig.Bucket), err, true,
		))
	}

	return len(objectKeys) - len(perKey), failures
}

/*
DeleteOrphanedStagingObjects reclaim the staging objects left behind by uploads that aborted
before their best-effort cleanup (see DESIGN §8.2.1 item 1).

A staging key never maps to an artifact entry by construction (see DESIGN §8.1), so there is
nothing to join against - every staging object is unassociated, and age alone decides whether
it is still in flight or abandoned. Nothing here reads the persistence layer.

The grace window must therefore outlast a WHOLE upload rather than just the copy -> insert gap
the storage sweep contends with: a staging object is live from the moment its PUT URL is minted
until registration copies it away. Configure `objAgeOutSec` above `putUrlTTL` plus the time
registration takes, or a slow upload is reclaimed out from under itself. The two settings are
independent and nothing checks the relationship between them.

	@param ctx context.Context - execution context
	@param timestamp time.Time - the current timestamp, which the grace window is measured back
	    from
	@param workspaceID *string - optionally, confine the sweep to one workspace's staging key
	    namespace; nil sweeps every workspace's
	@returns what the sweep observed and reclaimed
*/
func (m *managerImpl) DeleteOrphanedStagingObjects(
	ctx context.Context, timestamp time.Time, workspaceID *string,
) (StagingReapReport, error) {
	logTags := m.GetLogTagsForContext(ctx)

	var report StagingReapReport

	cutoff := m.orphanCutoff(timestamp)
	prefix := m.stagingKeyPrefix(workspaceID)

	var failures []error
	var startAfter *string
	for {
		objects, err := m.listObjectPage(ctx, prefix, startAfter)
		if err != nil {
			return report, models.NewMaintenanceError(
				"failed to reclaim orphaned staging objects", err, true,
			)
		}
		if len(objects) == 0 {
			break
		}

		report.Examined += len(objects)

		var reclaim []string
		for _, object := range objects {
			if !objectAgedOut(cutoff, object) {
				report.Retained++
				continue
			}
			reclaim = append(reclaim, object.Key)
		}

		deleted, deleteFailures := m.deleteObjects(ctx, reclaim)
		report.Deleted += deleted
		report.Retained += len(reclaim) - deleted
		failures = append(failures, deleteFailures...)

		if len(objects) < objectListPageSize {
			break
		}
		startAfter = &objects[len(objects)-1].Key
	}

	log.
		WithFields(logTags).
		WithField("examined", report.Examined).
		WithField("deleted", report.Deleted).
		Debug("Swept staging objects")

	if len(failures) > 0 {
		return report, models.NewMaintenanceError(
			fmt.Sprintf(
				"failed to reclaim %d of the %d staging objects examined",
				len(failures), report.Examined,
			), errors.Join(failures...), true,
		)
	}

	return report, nil
}

/*
ReconcileStorageObjects settle the artifact entries against the objects backing them, in both
directions (see DESIGN §8.2.1 items 2 and 3).

This is the sole object-deleter in the system. Deleting an artifact or a workspace is a plain
row delete that leaves the object untouched (see DESIGN §4.1), so a freed object only ever
reaches reclamation through here.

Both directions fall out of one pass over the entries, which is why they are not separate calls
- asking the same question twice would scan the store twice to learn the same thing.

	@param ctx context.Context - execution context
	@param timestamp time.Time - the current timestamp, which the grace window is measured back
	    from
	@param workspaceID *string - optionally, confine the reconciliation to one workspace's
	    entries and storage key namespace; nil reconciles every workspace's
	@returns what the reconciliation observed, reclaimed, and flagged
*/
func (m *managerImpl) ReconcileStorageObjects(
	ctx context.Context, timestamp time.Time, workspaceID *string,
) (StorageReconcileReport, error) {
	logTags := m.GetLogTagsForContext(ctx)

	var report StorageReconcileReport

	// One cutoff for both directions. The reclamations and the quarantines are two readings of
	// the same grace window (see DESIGN §8.2.1 item 3), so they must not be able to disagree
	// about where it falls.
	cutoff := m.orphanCutoff(timestamp)

	// Read the entries BEFORE listing the store, and the ordering is load-bearing. An entry is
	// only ever inserted AFTER its object copy completes (see DESIGN §6.1), so an entry read
	// here is guaranteed to have had its object already, and the later listing will find it.
	// Listing first inverts that: a registration completing between the two reads leaves an
	// entry pointing at an object the listing never returned, and the sweep would raise a
	// data-loss alarm against a perfectly healthy artifact.
	entries, err := m.readArtifactEntries(ctx, workspaceID)
	if err != nil {
		return report, models.NewMaintenanceError(
			"failed to reconcile storage objects", err, true,
		)
	}

	// Whether an object is claimed is answered by entries in EVERY state. A quarantined entry
	// still names its object key, and should that object ever reappear it is that artifact's
	// data being recovered, not garbage to reap.
	associated := make(map[string]bool, len(entries))

	// What the store failed to show is asked only of `RECORDED` entries, and that restriction
	// is what keeps an already-quarantined entry from being flagged again. The persistence
	// layer permits the `MISSING_OBJECT` -> `MISSING_OBJECT` self-transition and records an
	// audit event on every flag, so re-flagging would republish the same data-loss incident on
	// every sweep, forever.
	unobserved := make(map[string]models.Artifact, len(entries))

	for _, entry := range entries {
		associated[entry.ObjectKey] = true
		if entry.State == models.ArtifactStateRecorded {
			unobserved[entry.ObjectKey] = entry
		}
	}

	prefix := m.storeKeyPrefix(workspaceID)

	var failures []error
	var startAfter *string
	for {
		objects, err := m.listObjectPage(ctx, prefix, startAfter)
		if err != nil {
			return report, models.NewMaintenanceError(
				"failed to reconcile storage objects", err, true,
			)
		}
		if len(objects) == 0 {
			break
		}

		report.Examined += len(objects)

		var reclaim []string
		for _, object := range objects {
			if associated[object.Key] {
				// Claimed, so the entry holding it has now been seen backed. Nothing more is
				// asked of this key.
				delete(unobserved, object.Key)
				continue
			}

			// The join happening here, as the deletion is decided, IS the re-validation DESIGN
			// §8.3.1 asks for: a key goes only because it is STILL unclaimed on this pass.
			if !objectAgedOut(cutoff, object) {
				report.Retained++
				continue
			}
			reclaim = append(reclaim, object.Key)
		}

		deleted, deleteFailures := m.deleteObjects(ctx, reclaim)
		report.Deleted += deleted
		report.Retained += len(reclaim) - deleted
		failures = append(failures, deleteFailures...)

		if len(objects) < objectListPageSize {
			break
		}
		startAfter = &objects[len(objects)-1].Key
	}

	flagged, flagFailures := m.quarantineUnbackedArtifacts(ctx, cutoff, unobserved)
	report.FlaggedMissing = flagged
	failures = append(failures, flagFailures...)

	log.
		WithFields(logTags).
		WithField("examined", report.Examined).
		WithField("deleted", report.Deleted).
		WithField("flagged", len(report.FlaggedMissing)).
		Debug("Reconciled storage objects")

	if len(failures) > 0 {
		return report, models.NewMaintenanceError(
			fmt.Sprintf(
				"failed to fully reconcile the %d storage objects examined", report.Examined,
			), errors.Join(failures...), true,
		)
	}

	return report, nil
}

/*
readArtifactEntries read the artifact entries the reconciliation compares the store against.

Every state is read, not just `RECORDED`: the two questions the reconciliation asks need
different subsets, and taking the superset once is what lets a single dump answer both.

	@param ctx context.Context - execution context
	@param workspaceID *string - optionally, confine the read to one workspace's entries
	@returns the artifact entries
*/
func (m *managerImpl) readArtifactEntries(
	ctx context.Context, workspaceID *string,
) ([]models.Artifact, error) {
	var entries []models.Artifact

	if err := m.persistence.UseDatabaseInTransaction(
		ctx, func(dbCtx context.Context, dbClient db.Database) error {
			var err error
			entries, err = dbClient.ListArtifacts(
				dbCtx, db.ArtifactQueryFilter{WorkspaceID: workspaceID},
			)
			if err != nil {
				return goutils.NewPersistenceError("failed to list artifacts", err, true)
			}
			return nil
		},
	); err != nil {
		return nil, err
	}

	return entries, nil
}

/*
quarantineUnbackedArtifacts flag the artifact entries the store had no object for.

Each flag is its own transaction so one entry's failure can't roll back another's, and so the
state write stays atomic with the audit event persistence records alongside it.

Held to the same grace window the deletions use, per DESIGN §8.2.1 item 3. An entry written
moments ago is still in flux - its object may be landing as this runs - and quarantine is an
incident report, not a retryable correction. Only a settled entry makes a claim worth publishing.

	@param ctx context.Context - execution context
	@param cutoff time.Time - the grace window's trailing edge, the same one the reclamations
	    were gated on
	@param unobserved map[string]models.Artifact - the `RECORDED` entries the store never showed
	@returns the IDs of the entries quarantined
*/
func (m *managerImpl) quarantineUnbackedArtifacts(
	ctx context.Context, cutoff time.Time, unobserved map[string]models.Artifact,
) ([]string, []error) {
	logTags := m.GetLogTagsForContext(ctx)

	var flagged []string
	var failures []error

	for _, entry := range unobserved {
		if !entry.UpdatedAt.Before(cutoff) {
			continue
		}

		if err := m.persistence.UseDatabaseInTransaction(
			ctx, func(dbCtx context.Context, dbClient db.Database) error {
				if err := dbClient.MarkArtifactMissingObject(dbCtx, entry.ID); err != nil {
					return goutils.NewPersistenceError(
						fmt.Sprintf("failed to quarantine artifact %s", entry.ID), err, true,
					)
				}
				return nil
			},
		); err != nil {
			// The entry was deleted between the dump and the flag. An operator purging an
			// artifact mid-sweep is ordinary, and there is nothing left to quarantine.
			var notFound goutils.NotFoundError
			if errors.As(err, &notFound) {
				log.
					WithFields(logTags).
					WithField("artifact", entry.ID).
					Debug("Artifact deleted before it could be quarantined")
				continue
			}

			log.
				WithError(err).
				WithFields(logTags).
				WithField("artifact", entry.ID).
				WithField("object_key", entry.ObjectKey).
				Error("Failed to quarantine artifact whose object is gone")
			failures = append(failures, err)
			continue
		}

		log.
			WithFields(logTags).
			WithField("artifact", entry.ID).
			WithField("object_key", entry.ObjectKey).
			Warn("Quarantined artifact whose backing object is gone")
		flagged = append(flagged, entry.ID)
	}

	return flagged, failures
}

/*
stagingKeyPrefix build the staging key prefix the sweep lists on.

	@param workspaceID *string - optionally, the one workspace to confine the sweep to
	@returns the object key prefix
*/
func (m *managerImpl) stagingKeyPrefix(workspaceID *string) string {
	if workspaceID != nil {
		return sweepPrefix(m.storeConfig.Prefix.WorkspaceStagingKeyPrefix(*workspaceID))
	}
	return sweepPrefix(m.storeConfig.Prefix.StagingKeyPrefix())
}

/*
storeKeyPrefix build the final storage key prefix the reconciliation lists on.

	@param workspaceID *string - optionally, the one workspace to confine the reconciliation to
	@returns the object key prefix
*/
func (m *managerImpl) storeKeyPrefix(workspaceID *string) string {
	if workspaceID != nil {
		return sweepPrefix(m.storeConfig.Prefix.WorkspaceStoreKeyPrefix(*workspaceID))
	}
	return sweepPrefix(m.storeConfig.Prefix.StoreKeyPrefix())
}

// sweepPrefix close a key prefix onto a path segment boundary.
//
// The prefix helpers join path segments and so return no trailing separator, which as a listing
// filter is a plain string match: `<base>/staging` would also select `<base>/staging-archive/…`
// and hand a neighbouring prefix's objects to a sweep that deletes what it finds. The same
// closing is what makes the staging key ownership check in the artifact manager sound.
func sweepPrefix(prefix string) string {
	return prefix + "/"
}
