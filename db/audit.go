// Package db - database controllers for system persistence
package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/oklog/ulid/v2"
	"gorm.io/datatypes"
)

// defineNewSystemEvent record a new system event.
//
// Called from within the same transaction as the change it records, so an event and the
// operation it describes are committed or rolled back together — the audit trail can never
// disagree with the entries it describes.
func (c *databaseImpl) defineNewSystemEvent(
	_ context.Context, eventType models.SystemEventTypeENUM, metadata interface{},
) (models.SystemEventAudit, error) {
	if err := c.validator.Struct(metadata); err != nil {
		return models.SystemEventAudit{}, goutils.NewValidationError(
			fmt.Sprintf("new system event '%s' metadata entry is not valid", eventType), err, true,
		)
	}

	metadataStr, _ := json.Marshal(&metadata)

	newEntry := SystemEventAuditEntry{
		SystemEventAudit: models.SystemEventAudit{
			ID:        ulid.Make().String(),
			EventType: eventType,
			Metadata:  datatypes.JSON(metadataStr),
		},
	}

	if err := c.validator.Struct(&newEntry); err != nil {
		return models.SystemEventAudit{}, goutils.NewValidationError(
			fmt.Sprintf("new system event '%s' entry is not valid", eventType), err, true,
		)
	}

	if tmp := c.db.Create(&newEntry); tmp.Error != nil {
		return models.SystemEventAudit{}, goutils.NewSQLError(
			fmt.Sprintf("new system event '%s' insert failed", eventType), tmp.Error, true,
		)
	}

	return newEntry.SystemEventAudit, nil
}

/*
ListSystemEvents list captured system events

	@param ctx context.Context - execution context
	@param filters SystemEventQueryFilter - entry listing filter
	@return list of system events
*/
func (c *databaseImpl) ListSystemEvents(
	_ context.Context, filters SystemEventQueryFilter,
) ([]models.SystemEventAudit, error) {
	if err := c.validator.Struct(&filters); err != nil {
		return nil, goutils.NewValidationError("system event query filter is not valid", err, true)
	}

	query := c.db.Model(&SystemEventAuditEntry{})

	if len(filters.EventTypes) > 0 {
		query = query.Where("type in ?", filters.EventTypes)
	}

	if filters.EventsAfter != nil {
		query = query.Where("created_at >= ?", *filters.EventsAfter)
	}
	if filters.EventsBefore != nil {
		query = query.Where("created_at <= ?", *filters.EventsBefore)
	}

	if filters.Limit != nil {
		query = query.Limit(*filters.Limit)
	}
	if filters.Offset != nil {
		query = query.Offset(*filters.Offset)
	}

	// The ID is a ULID, so ordering by it is creation ordering — stable even for events
	// recorded within the same `created_at` tick.
	query = query.Order("id")

	var entries []SystemEventAuditEntry
	if tmp := query.Find(&entries); tmp.Error != nil {
		return nil, goutils.NewSQLError("failed to list captured system events", tmp.Error, true)
	}

	result := []models.SystemEventAudit{}
	for _, entry := range entries {
		result = append(result, entry.SystemEventAudit)
	}

	return result, nil
}
