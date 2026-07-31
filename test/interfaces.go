// Package test - various support components used in unit-testing.
package test

// UnitTestCallbackCollector unit-testing interface for collecting callbacks
type UnitTestCallbackCollector interface {
	// EstimateMIMEType called when artifact manager to need to estimate data MIME type
	EstimateMIMEType(data []byte) string
}
