// Package models - application data models
package models

import (
	"reflect"
	"regexp"

	"github.com/alwitt/goutils"
	"github.com/go-playground/validator/v10"
)

/*
RegisterWithValidator register with the validator this custom validation support

	@param v *validator.Validate - the validator to register against
	@return whether successful
*/
func RegisterWithValidator(v *validator.Validate) error {
	if err := goutils.RegisterENUMInValidator(
		v, "volume_state", goutils.ValidateStringENUM[WorkspaceVolumeStateENUM](),
	); err != nil {
		return err
	}

	if err := goutils.RegisterENUMInValidator(
		v, "artifact_state", goutils.ValidateStringENUM[ArtifactStateENUM](),
	); err != nil {
		return err
	}

	if err := goutils.RegisterENUMInValidator(
		v, "system_event_type", goutils.ValidateStringENUM[SystemEventTypeENUM](),
	); err != nil {
		return err
	}

	if err := goutils.RegisterENUMInValidator(
		v, "workspace_volume_type", goutils.ValidateStringENUM[WorkspaceVolumeTypeENUM](),
	); err != nil {
		return err
	}

	if err := v.RegisterValidation(
		"valid_name", validateNameType,
	); err != nil {
		return err
	}

	return goutils.RegisterWithValidator(v)
}

var validNameREGEX = regexp.MustCompile(`^[a-zA-Z0-9-_]+$`)

func validateNameType(fl validator.FieldLevel) bool {
	if fl.Field().Kind() != reflect.String {
		return false
	}
	return validNameREGEX.MatchString(fl.Field().String())
}
