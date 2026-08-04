package maintenance_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alwitt/cairn/db"
	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// unitTestListPageSize the page size the sweeps ask the object store for. Asserted as a literal
// rather than read from the package, so the value reaching the object store is pinned
// independently of the constant that produces it.
const unitTestListPageSize = 1000

// firstPage the `startAfter` a first listing carries - there is no previous page to resume from.
var firstPage *string

// agedOut an age comfortably past the harness grace window of one hour.
const agedOut = 2 * time.Hour

// stillFresh an age comfortably inside the harness grace window.
const stillFresh = 5 * time.Minute

// listedObject an object as the store's listing reports it - a key, and how long ago it was
// last written.
type listedObject struct {
	key string
	age time.Duration
}

// aged describe an object the listing reports as last written this long ago.
func aged(key string, age time.Duration) listedObject {
	return listedObject{key: key, age: age}
}

// expectObjectPage arrange one listing page, matching the prefix and resume key exactly.
//
// The listing carries each object's age, which is the whole point of the shape: nowhere in
// these tests is `GetObjectStat` ever arranged, so a sweep that fell back to a per-object stat
// to age anything would fail every case in this file.
func expectObjectPage(
	mocks unitTestMocks, prefix string, startAfter *string, objects ...listedObject,
) {
	pageSize := unitTestListPageSize

	page := make([]goutils.S3ObjectStat, 0, len(objects))
	for _, object := range objects {
		page = append(page, goutils.S3ObjectStat{
			Key: object.key,
			// A listing reports neither MIMEType nor CheckSum, so they are deliberately left
			// zero here - a sweep must not come to depend on either.
			LastModified: time.Now().UTC().Add(-object.age),
		})
	}

	mocks.objects.EXPECT().
		ListObjects(mock.Anything, unitTestBucket, &prefix, startAfter, &pageSize).
		Return(page, nil).
		Once()
}

// expectObjectDelete arrange a successful bulk reclamation of exactly these keys.
func expectObjectDelete(mocks unitTestMocks, keys ...string) {
	mocks.objects.EXPECT().
		DeleteObjects(mock.Anything, unitTestBucket, keys).
		Return(map[string]error{}, nil).
		Once()
}

// expectArtifactListing arrange the artifact dump the storage reconciliation opens with.
func expectArtifactListing(
	mocks unitTestMocks, workspaceID *string, entries []models.Artifact,
) {
	mocks.database.EXPECT().
		ListArtifacts(mock.Anything, db.ArtifactQueryFilter{WorkspaceID: workspaceID}).
		Return(entries, nil).
		Once()
}

// sampleArtifact build an artifact entry of the shape persistence returns, settled the given
// duration ago.
func sampleArtifact(
	objectKey string, state models.ArtifactStateENUM, settledFor time.Duration,
) models.Artifact {
	return models.Artifact{
		ID:          ulid.Make().String(),
		WorkspaceID: uuid.NewString(),
		Name:        "artifact-" + objectKey,
		ObjectKey:   objectKey,
		State:       state,
		UpdatedAt:   time.Now().UTC().Add(-settledFor),
	}
}

/*
TestDeleteOrphanedStagingObjects validates the staging sweep of DESIGN §8.2.1 item 1 - the
reclamation of objects left behind by uploads that aborted before their own cleanup.

A staging key never maps to an artifact entry, so age is the only thing separating an upload
still in flight from one abandoned. The sweep is correspondingly narrow: object store only.
*/
func TestDeleteOrphanedStagingObjects(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: the grace window decides, one object at a time, off the age the listing already
	// reported. The young object is what the window exists for - it may be an upload
	// mid-flight, and reclaiming it would destroy live data rather than garbage.
	t.Run("reclaims the aged and retains the fresh", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		expectObjectPage(
			mocks, "staging/", firstPage,
			aged("staging/ws/old", agedOut),
			aged("staging/ws/new", stillFresh),
		)
		expectObjectDelete(mocks, "staging/ws/old")

		report, err := manager.DeleteOrphanedStagingObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal(2, report.Examined)
		assert.Equal(1, report.Deleted)
		assert.Equal(1, report.Retained)
	})

	// Case 2: persistence is never consulted. Neither the transaction client nor the database
	// has a single expectation arranged, so any read at all fails the case.
	//
	// This is the whole reason staging and storage are separate methods: a staging key is
	// unassociated by construction, so there is no join to perform and no artifact dump to pay
	// for. A sweep that queried anyway would be doing work that can only ever return nothing.
	t.Run("never consults persistence", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		expectObjectPage(mocks, "staging/", firstPage, aged("staging/ws/old", agedOut))
		expectObjectDelete(mocks, "staging/ws/old")

		_, err := manager.DeleteOrphanedStagingObjects(utCtx, nil)
		assert.Nil(err)
	})

	// Case 3: the prefix is closed onto a path segment boundary. Listing on the bare `staging`
	// would also select `staging-archive/...`, and this sweep deletes what it finds - a missing
	// separator would hand a neighbouring prefix's objects to a reaper.
	t.Run("lists on a separator-closed prefix", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		expectObjectPage(mocks, "staging/", firstPage)

		report, err := manager.DeleteOrphanedStagingObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal(0, report.Examined)
	})

	// Case 4: a workspace scope narrows the prefix to that workspace's namespace, which is what
	// lets an operator reclaim one workspace promptly without sweeping the bucket.
	t.Run("narrows the prefix to a scoped workspace", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		workspaceID := uuid.NewString()

		expectObjectPage(mocks, fmt.Sprintf("staging/%s/", workspaceID), firstPage)

		_, err := manager.DeleteOrphanedStagingObjects(utCtx, &workspaceID)
		assert.Nil(err)
	})

	// Case 5: a full page means there may be more, so the sweep resumes from its last key. A
	// sweep that stopped at one page would silently leave everything past the first 1000 keys
	// to accumulate forever.
	//
	// Everything on both pages is fresh, so nothing is reclaimed and the paging is what the
	// case is left measuring. A full page costs exactly one object store call - no per-object
	// traffic is arranged, so any would fail the case.
	t.Run("resumes paging after a full page", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		fullPage := make([]listedObject, unitTestListPageSize)
		for i := range fullPage {
			fullPage[i] = aged(fmt.Sprintf("staging/ws/%04d", i), stillFresh)
		}
		lastKey := fullPage[len(fullPage)-1].key

		expectObjectPage(mocks, "staging/", firstPage, fullPage...)
		expectObjectPage(mocks, "staging/", &lastKey, aged("staging/ws/tail", stillFresh))

		report, err := manager.DeleteOrphanedStagingObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal(unitTestListPageSize+1, report.Examined)
		assert.Equal(0, report.Deleted)
		assert.Equal(unitTestListPageSize+1, report.Retained)
	})

	// Case 6: a failed listing aborts rather than reporting an empty sweep. The listing is now
	// the sole source of both what exists and how old it is, so losing it leaves the sweep with
	// nothing to reason about at all.
	t.Run("aborts when the listing fails", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		listFailure := fmt.Errorf("object store is unreachable")

		mocks.objects.EXPECT().
			ListObjects(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, listFailure).
			Once()

		report, err := manager.DeleteOrphanedStagingObjects(utCtx, nil)
		assert.NotNil(err)
		assert.ErrorIs(err, listFailure)
		assert.Equal(0, report.Examined)
	})

	// Case 7: a per-key reclamation failure is surfaced without stopping the batch. The keys
	// that did go are gone whatever happened to the rest, and the next sweep re-derives the one
	// that stayed.
	t.Run("surfaces a per-key reclamation failure", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		deleteFailure := fmt.Errorf("access denied")

		expectObjectPage(
			mocks, "staging/", firstPage,
			aged("staging/ws/a", agedOut),
			aged("staging/ws/b", agedOut),
		)
		mocks.objects.EXPECT().
			DeleteObjects(mock.Anything, unitTestBucket, []string{"staging/ws/a", "staging/ws/b"}).
			Return(map[string]error{"staging/ws/b": deleteFailure}, nil).
			Once()

		report, err := manager.DeleteOrphanedStagingObjects(utCtx, nil)
		assert.NotNil(err)
		assert.ErrorIs(err, deleteFailure)
		assert.Equal(1, report.Deleted)
		assert.Equal(1, report.Retained)
	})
}

/*
TestReconcileStorageObjects validates the two-directional storage reconciliation of DESIGN
§8.2.1 items 2 and 3 - the only place in the system an artifact's backing object is ever
deleted, and the only place a lost one is ever reported.

One artifact dump answers both questions, and the state an entry is in decides which of the two
it participates in.
*/
func TestReconcileStorageObjects(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	utCtx := context.Background()

	// Case 1: an object no entry references, past the grace window, is reclaimed. This is the
	// normal aftermath of a purge - the row went first and left the object behind (see DESIGN
	// §4.1).
	t.Run("reclaims an unreferenced aged object", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{})
		expectObjectPage(mocks, "store/", firstPage, aged("store/ws/purged", agedOut))
		expectObjectDelete(mocks, "store/ws/purged")

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal(1, report.Examined)
		assert.Equal(1, report.Deleted)
		assert.Empty(report.FlaggedMissing)
	})

	// Case 2: an unreferenced object inside the grace window is left alone, and `DeleteObjects`
	// is not arranged so reclaiming it fails the case.
	//
	// This is the window's entire purpose. A registration copies its object and only then
	// inserts the row, so an upload mid-flight looks exactly like a purged orphan; the window
	// is the only thing telling them apart.
	t.Run("retains an unreferenced object inside the window", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{})
		expectObjectPage(mocks, "store/", firstPage, aged("store/ws/inflight", stillFresh))

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal(1, report.Retained)
		assert.Equal(0, report.Deleted)
	})

	// Case 3: a live entry's object is kept no matter how old it is. The object here is well
	// past the grace window, so age alone would condemn it - being referenced is what saves it,
	// and that has to be the test the sweep applies first.
	t.Run("keeps a referenced object however aged", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		entry := sampleArtifact("store/ws/live", models.ArtifactStateRecorded, agedOut)

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{entry})
		expectObjectPage(mocks, "store/", firstPage, aged(entry.ObjectKey, agedOut))

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal(1, report.Examined)
		assert.Equal(0, report.Deleted)
		assert.Empty(report.FlaggedMissing)
	})

	// Case 4: a quarantined entry still claims its object key, so an object that reappears
	// under it is that artifact's data being recovered, not garbage. Testing whether an object
	// is claimed has to span every state, not just the live one.
	t.Run("keeps an object claimed by a quarantined entry", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		entry := sampleArtifact("store/ws/recovered", models.ArtifactStateMissingObject, agedOut)

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{entry})
		expectObjectPage(mocks, "store/", firstPage, aged(entry.ObjectKey, agedOut))

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal(0, report.Deleted)
	})

	// Case 5: the other direction. A live entry the store had no object for has lost its data,
	// and the entry is quarantined as evidence rather than deleted (see DESIGN §2.2.1).
	t.Run("quarantines a live entry with no object", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		entry := sampleArtifact("store/ws/lost", models.ArtifactStateRecorded, agedOut)

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{entry})
		expectObjectPage(mocks, "store/", firstPage)

		mocks.database.EXPECT().
			MarkArtifactMissingObject(mock.Anything, entry.ID).
			Return(nil).
			Once()

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal([]string{entry.ID}, report.FlaggedMissing)
	})

	// Case 6: an already-quarantined entry is not quarantined again.
	// `MarkArtifactMissingObject` is not arranged, so a second flag fails the case.
	//
	// Nothing downstream would stop it: the self-transition is permitted. But persistence
	// records an audit event on every flag, so re-flagging would republish the same data-loss
	// incident on every sweep for as long as the entry exists.
	t.Run("does not requarantine an already-flagged entry", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		entry := sampleArtifact("store/ws/known-lost", models.ArtifactStateMissingObject, agedOut)

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{entry})
		expectObjectPage(mocks, "store/", firstPage)

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Empty(report.FlaggedMissing)
	})

	// Case 7: an entry written moments ago is left alone. Quarantine is an incident report
	// rather than a retryable correction, so only a settled entry makes a claim worth
	// publishing - an entry still in flux may simply have its object landing right now.
	t.Run("does not quarantine an unsettled entry", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		entry := sampleArtifact("store/ws/just-written", models.ArtifactStateRecorded, stillFresh)

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{entry})
		expectObjectPage(mocks, "store/", firstPage)

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Empty(report.FlaggedMissing)
	})

	// Case 8: the entries are read BEFORE the store is listed, and the ordering is load-bearing.
	// An entry is only inserted after its object copy completes, so an entry read first is
	// guaranteed to have had its object and the later listing will find it. Listing first
	// inverts that - a registration completing between the two reads leaves an entry pointing
	// at an object the listing never returned, and the sweep would raise a data-loss alarm
	// against a perfectly healthy artifact.
	t.Run("reads entries before listing the store", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)

		var order []string

		expectTransactions(mocks)
		mocks.database.EXPECT().
			ListArtifacts(mock.Anything, mock.Anything).
			RunAndReturn(
				func(context.Context, db.ArtifactQueryFilter) ([]models.Artifact, error) {
					order = append(order, "entries")
					return []models.Artifact{}, nil
				},
			).
			Once()
		mocks.objects.EXPECT().
			ListObjects(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(
				func(
					context.Context, string, *string, *string, *int,
				) ([]goutils.S3ObjectStat, error) {
					order = append(order, "objects")
					return []goutils.S3ObjectStat{}, nil
				},
			).
			Once()

		_, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal([]string{"entries", "objects"}, order)
	})

	// Case 9: a failed entry dump aborts before the store is touched. `ListObjects` is not
	// arranged, so listing fails the case - without the entries there is no way to tell an
	// orphan from live data, and every object would look unreferenced.
	t.Run("aborts before listing when the entry dump fails", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		dbFailure := fmt.Errorf("database is unreachable")

		expectTransactions(mocks)
		mocks.database.EXPECT().
			ListArtifacts(mock.Anything, mock.Anything).
			Return(nil, dbFailure).
			Once()

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.NotNil(err)
		assert.ErrorIs(err, dbFailure)
		assert.Equal(0, report.Examined)
	})

	// Case 10: one entry failing to quarantine does not withhold the others, and the report
	// still names the one that landed.
	t.Run("keeps quarantining after one entry fails", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		doomed := sampleArtifact("store/ws/lost-a", models.ArtifactStateRecorded, agedOut)
		healthy := sampleArtifact("store/ws/lost-b", models.ArtifactStateRecorded, agedOut)
		writeFailure := fmt.Errorf("row is locked")

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{doomed, healthy})
		expectObjectPage(mocks, "store/", firstPage)

		mocks.database.EXPECT().
			MarkArtifactMissingObject(mock.Anything, doomed.ID).
			Return(writeFailure).
			Once()
		mocks.database.EXPECT().
			MarkArtifactMissingObject(mock.Anything, healthy.ID).
			Return(nil).
			Once()

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.NotNil(err)
		assert.ErrorIs(err, writeFailure)
		assert.Equal([]string{healthy.ID}, report.FlaggedMissing)
	})

	// Case 11: an entry purged between the dump and the flag is an ordinary outcome of
	// reconciling against a live system, not a failure. An operator deleting an artifact
	// mid-sweep must not make an unattended sweep raise an alarm.
	t.Run("tolerates an entry purged mid-sweep", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		entry := sampleArtifact("store/ws/purged-midway", models.ArtifactStateRecorded, agedOut)

		expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{entry})
		expectObjectPage(mocks, "store/", firstPage)

		mocks.database.EXPECT().
			MarkArtifactMissingObject(mock.Anything, entry.ID).
			Return(goutils.NewNotFoundError("artifact does not exist", nil, true)).
			Once()

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Empty(report.FlaggedMissing)
	})

	// Case 12: both directions in one pass, each landing in its own bucket, off a single
	// artifact dump. This is what makes the two a single call rather than two scans of the same
	// store.
	t.Run("reclaims and quarantines in one pass", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		live := sampleArtifact("store/ws/live", models.ArtifactStateRecorded, agedOut)
		lost := sampleArtifact("store/ws/lost", models.ArtifactStateRecorded, agedOut)

		transactions := expectTransactions(mocks)
		expectArtifactListing(mocks, nil, []models.Artifact{live, lost})
		expectObjectPage(
			mocks, "store/", firstPage,
			aged(live.ObjectKey, agedOut),
			aged("store/ws/orphan", agedOut),
		)
		expectObjectDelete(mocks, "store/ws/orphan")

		mocks.database.EXPECT().
			MarkArtifactMissingObject(mock.Anything, lost.ID).
			Return(nil).
			Once()

		report, err := manager.ReconcileStorageObjects(utCtx, nil)
		assert.Nil(err)
		assert.Equal(2, report.Examined)
		assert.Equal(1, report.Deleted)
		assert.Equal([]string{lost.ID}, report.FlaggedMissing)

		// One transaction for the dump plus one per quarantine - a failed flag can't roll back
		// another entry's.
		assert.Equal(2, *transactions)
	})

	// Case 13: a workspace scope narrows both halves together - the entries queried and the
	// prefix listed. Narrowing only one would compare a workspace's objects against every
	// workspace's entries, or the reverse, and reap live data.
	t.Run("narrows both the entries and the prefix when scoped", func(t *testing.T) {
		assert := assert.New(t)

		manager, mocks := newUnitTestManager(t)
		workspaceID := uuid.NewString()

		expectTransactions(mocks)
		expectArtifactListing(mocks, &workspaceID, []models.Artifact{})
		expectObjectPage(mocks, fmt.Sprintf("store/%s/", workspaceID), firstPage)

		_, err := manager.ReconcileStorageObjects(utCtx, &workspaceID)
		assert.Nil(err)
	})
}
