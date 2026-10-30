//go:build !linux && !darwin && !windows

package caffeine

import "errors"

func hold() (func(), error) {
	return nil, errors.New("keeping the host awake is not supported on this platform")
}
