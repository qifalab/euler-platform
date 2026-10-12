// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

//go:build !linux && !darwin

package action

import (
	"errors"
	"io/fs"
	"os"
)

type fileObjectGenerationValue struct {
	Kind  string
	Token string
}

func fileObjectGeneration(_ *os.File, _ fs.FileInfo, _ string) (fileObjectGenerationValue, error) {
	return fileObjectGenerationValue{}, errors.New("stable file generation guards are unavailable on this platform")
}
