// Package vsock listens on AF_VSOCK sockets inside a Linux guest with raw
// system calls, so the guest helper needs no extra dependency. It builds only
// on Linux; the package is empty on other operating systems.
package vsock
