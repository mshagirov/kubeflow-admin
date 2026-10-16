package config

import "path"

// A static import must be confined to an explicitly prepared non-root directory,
// rather than approving every export on the server through basePath: /.
func validNFSBasePath(base string) bool {
	return base != "/" && path.IsAbs(base) && path.Clean(base) == base
}
