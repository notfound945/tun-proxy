//go:build !darwin || !cgo

package procattrib

import "errors"

func Lookup(Flow) (Result, error) {
	return Result{}, errors.New("process attribution spike requires macOS with cgo enabled")
}
