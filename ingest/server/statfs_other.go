//go:build !unix

package server

func filesystemSize(string) int64 { return 0 }
