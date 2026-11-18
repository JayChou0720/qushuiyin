// Package fixtures contains synthetic contract examples, never live measurements.
package fixtures

import _ "embed"

//go:embed samples.json
var Samples []byte
