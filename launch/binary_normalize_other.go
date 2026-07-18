//go:build !windows

package launch

func normalizeResolvedExecutable(path string) (resolvedExecutable, error) {
	return resolvedExecutable{path: path}, nil
}
