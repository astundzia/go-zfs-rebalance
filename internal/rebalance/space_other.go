//go:build !linux && !darwin

package rebalance

import (
	"os"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
)

func folderFreeBytes(*os.Root, string) (uint64, error) { return 0, fileutil.ErrUnsupportedPlatform }
