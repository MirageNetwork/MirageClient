// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package synology

import "tailscale.com/release/dist"

var v5Models = []string{
	"armv5",
	"88f6281",
	"88f6282",
	// hi3535 is actually an armv7 under the hood, but with no
	// hardware floating point. To the Go compiler, that means it's an
	// armv5.
	"hi3535",
}

var v7Models = []string{
	"armv7",
	"alpine",
	"armada370",
	"armada375",
	"armada38x",
	"armadaxp",
	"comcerto2k",
	"monaco",
}

func Targets(forPackageCenter bool) []dist.Target {
	var ret []dist.Target
	for _, dsmVersion := range []int{6, 7} {
		ret = append(ret,
			&target{
				filenameArch:    "x86_64",
				dsmMajorVersion: dsmVersion,
				goenv: map[string]string{
					"GOOS":   "linux",
					"GOARCH": "amd64",
				},
				packageCenter: forPackageCenter,
			},
			&target{
				filenameArch:    "i686",
				dsmMajorVersion: dsmVersion,
				goenv: map[string]string{
					"GOOS":   "linux",
					"GOARCH": "386",
				},
				packageCenter: forPackageCenter,
			},
			&target{
				filenameArch:    "armv8",
				dsmMajorVersion: dsmVersion,
				goenv: map[string]string{
					"GOOS":   "linux",
					"GOARCH": "arm64",
				},
				packageCenter: forPackageCenter,
			})

		// On older ARMv5 and ARMv7 platforms, synology used a whole
		// mess of SoC-specific target names, even though the packages
		// built for each are identical apart from metadata.
		for _, v5Arch := range v5Models {
			ret = append(ret, &target{
				filenameArch:    v5Arch,
				dsmMajorVersion: dsmVersion,
				goenv: map[string]string{
					"GOOS":   "linux",
					"GOARCH": "arm",
					"GOARM":  "5",
				},
				packageCenter: forPackageCenter,
			})
		}
		for _, v7Arch := range v7Models {
			ret = append(ret, &target{
				filenameArch:    v7Arch,
				dsmMajorVersion: dsmVersion,
				goenv: map[string]string{
					"GOOS":   "linux",
					"GOARCH": "arm",
					"GOARM":  "7",
				},
				packageCenter: forPackageCenter,
			})
		}
	}
	return ret
}
