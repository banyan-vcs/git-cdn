// Package buildinfo carries the build identity. The linker overrides
// Version in image builds; plain source builds report "dev".
package buildinfo

var Version = "dev"
