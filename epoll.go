//go:build linux
// +build linux

package bigws

type eventLoop struct {
	epfd int
}
