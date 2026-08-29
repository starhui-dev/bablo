// Package cpa is the only Bablo package allowed to import CPA SDK packages.
//
// The package owns CPA service construction, lifecycle, protocol mapping, error
// classification, and stream cancellation. Business packages depend only on
// package inference and never receive CPA SDK values.
package cpa
