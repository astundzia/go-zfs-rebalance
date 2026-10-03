//go:build !linux && !darwin

package rebalance

import (
	"os"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
)

func openDir(*os.Root, string) (*os.File, error) { return nil, fileutil.ErrUnsupportedPlatform }
