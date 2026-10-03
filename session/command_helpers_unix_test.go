//go:build darwin || linux

package session

import "time"

type workbenchToken struct {
	Token string
	Since time.Time
}
