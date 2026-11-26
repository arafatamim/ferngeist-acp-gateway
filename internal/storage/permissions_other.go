//go:build !unix

package storage

func protectDatabaseFiles(string) error {
	return nil // File modes do not alter Windows ACLs.
}
