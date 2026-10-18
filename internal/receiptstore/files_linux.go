//go:build linux

package receiptstore

import (
	"errors"
	"io/fs"
	"os"
)

// withStorageRoot confines catalog-relative filesystem operations. It does not
// acquire or release the member's writer lock or authorize generation removal.
func withStorageRoot(directory string, operation func(*os.Root) error) (resultErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	return operation(root)
}

func storageStat(root, name string) (info fs.FileInfo, resultErr error) {
	resultErr = withStorageRoot(root, func(r *os.Root) error { var err error; info, err = r.Stat(name); return err })
	return info, resultErr
}

func storageLstat(root, name string) (info fs.FileInfo, resultErr error) {
	resultErr = withStorageRoot(root, func(r *os.Root) error { var err error; info, err = r.Lstat(name); return err })
	return info, resultErr
}

func openStorageFile(root, name string, flags int, mode fs.FileMode) (file *os.File, resultErr error) {
	resultErr = withStorageRoot(root, func(r *os.Root) error {
		var err error
		file, err = r.OpenFile(name, flags, mode)
		return err
	})
	// A root descriptor close failure must not leak an otherwise opened file.
	if resultErr != nil && file != nil {
		resultErr = errors.Join(resultErr, file.Close())
		file = nil
	}
	return file, resultErr
}

func syncMemberRoot(directory string) error {
	return withStorageRoot(directory, func(r *os.Root) error {
		file, err := r.Open(".")
		if err != nil {
			return err
		}
		return errors.Join(file.Sync(), file.Close())
	})
}

func storageMkdir(root, name string, mode fs.FileMode) error {
	return withStorageRoot(root, func(r *os.Root) error { return r.Mkdir(name, mode) })
}
