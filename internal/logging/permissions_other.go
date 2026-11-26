//go:build !unix

package logging

import "os"

func (s *Service) openActiveLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, s.fileMode)
}

func (s *Service) restrictBackups() error {
	return nil // File modes do not alter Windows ACLs.
}
