package utils

import (
	"os"
)

func JoinWorkdir(workdir, rel string) string {
	if workdir == "" {
		return rel
	}
	if rel == "" {
		return workdir
	}
	return workdir + string(os.PathSeparator) + rel
}

func Truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
